/*
Copyright 2021 Pyrra Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"fmt"
	"time"

	kitlog "github.com/go-kit/log"
	"github.com/go-kit/log/level"
	grafanav1beta1 "github.com/grafana/grafana-operator/v5/api/v1beta1"
	mimircli "github.com/grafana/mimir/pkg/mimirtool/client"
	"github.com/grafana/mimir/pkg/mimirtool/rules/rwrulefmt"
	"github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/rulefmt"
	yamlv3 "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/json"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	pyrrav1alpha1 "github.com/pyrra-dev/pyrra/kubernetes/api/v1alpha1"
	"github.com/pyrra-dev/pyrra/slo"
)

// ServiceLevelObjectiveReconciler reconciles a ServiceLevelObjective object.
type ServiceLevelObjectiveReconciler struct {
	client.Client
	MimirClient             *mimircli.MimirClient
	MimirWriteAlertingRules bool
	Logger                  kitlog.Logger
	Scheme                  *runtime.Scheme
	ConfigMapMode           bool
	GenericRules            bool
}

// +kubebuilder:rbac:groups=pyrra.dev,resources=servicelevelobjectives,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pyrra.dev,resources=servicelevelobjectives/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules/status,verbs=get

func (r *ServiceLevelObjectiveReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := kitlog.With(r.Logger, "reconciler", "servicelevelobjective", "namespace", req.NamespacedName)
	level.Debug(logger).Log("msg", "reconciling")

	var slo pyrrav1alpha1.ServiceLevelObjective
	if err := r.Get(ctx, req.NamespacedName, &slo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(fmt.Errorf("getting SLO: %w", err))
	}

	if r.ConfigMapMode {
		return r.reconcileConfigMap(ctx, logger, req, slo)
	}

	resp, err := r.reconcileMimirRuleGroup(ctx, logger, req, slo)
	if err != nil {
		return resp, err
	}

	return r.reconcilePrometheusRule(ctx, logger, req, slo)
}

func (r *ServiceLevelObjectiveReconciler) reconcilePrometheusRule(ctx context.Context, logger kitlog.Logger, req ctrl.Request, kubeObjective pyrrav1alpha1.ServiceLevelObjective) (ctrl.Result, error) {
	newRule, err := makePrometheusRule(kubeObjective, r.GenericRules)
	if err != nil {
		return ctrl.Result{}, err
	}

	var rule monitoringv1.PrometheusRule
	if err := r.Get(ctx, req.NamespacedName, &rule); err != nil {
		if errors.IsNotFound(err) {
			level.Info(logger).Log("msg", "creating prometheus rule", "namespace", rule.GetNamespace(), "name", rule.GetName())
			if err := r.Create(ctx, newRule); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to get prometheus rule: %w", err)
		}
	}

	newRule.ResourceVersion = rule.ResourceVersion

	level.Info(logger).Log("msg", "updating prometheus rule", "namespace", rule.GetNamespace(), "name", rule.GetName())
	if err := r.Update(ctx, newRule); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update prometheus rule: %w", err)
	}

	kubeObjective.Status.Type = "PrometheusRule"
	if err := r.Status().Update(ctx, &kubeObjective); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// TODO Implement deletion
func (r *ServiceLevelObjectiveReconciler) reconcileMimirRuleGroup(ctx context.Context, logger kitlog.Logger, req ctrl.Request, kubeObjective pyrrav1alpha1.ServiceLevelObjective) (ctrl.Result, error) {
	newRuleGroup, err := makeMimirRuleGroup(kubeObjective, r.GenericRules, r.MimirWriteAlertingRules)
	if err != nil {
		return ctrl.Result{}, err
	}

	level.Info(logger).Log("msg", "updating mimir rule", "name", newRuleGroup.Name)

	if err := r.Status().Update(ctx, &kubeObjective); err != nil {
		return ctrl.Result{}, err
	}

	// create rules
	kubeObjective.Status.Type = "MimirRule"
	err = r.MimirClient.CreateRuleGroup(ctx, kubeObjective.GetName(), *newRuleGroup)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ServiceLevelObjectiveReconciler) reconcileConfigMap(
	ctx context.Context,
	logger kitlog.Logger,
	req ctrl.Request,
	kubeObjective pyrrav1alpha1.ServiceLevelObjective,
) (ctrl.Result, error) {
	name := fmt.Sprintf("pyrra-recording-rule-%s", kubeObjective.GetName())

	newConfigMap, err := makeConfigMap(name, kubeObjective, r.GenericRules)
	if err != nil {
		return ctrl.Result{}, err
	}

	var existingConfigMap corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: req.Namespace,
		Name:      name,
	}, &existingConfigMap); err != nil {
		if errors.IsNotFound(err) {
			level.Info(logger).Log("msg", "creating config map", "namespace", newConfigMap.GetNamespace(), "name", newConfigMap.GetName())
			if err := r.Create(ctx, newConfigMap); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to create config map: %w", err)
			}
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to get config map: %w", err)
		}
	}

	newConfigMap.ResourceVersion = existingConfigMap.ResourceVersion

	level.Info(logger).Log("msg", "updating config map", "namespace", newConfigMap.GetNamespace(), "name", newConfigMap.GetName())
	if err := r.Update(ctx, newConfigMap); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update config map: %w", err)
	}

	kubeObjective.Status.Type = "ConfigMap"
	if err := r.Status().Update(ctx, &kubeObjective); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ServiceLevelObjectiveReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pyrrav1alpha1.ServiceLevelObjective{}).
		Complete(r)
}

func (r *ServiceLevelObjectiveReconciler) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&pyrrav1alpha1.ServiceLevelObjective{}).
		Complete()
}

func makeConfigMap(name string, kubeObjective pyrrav1alpha1.ServiceLevelObjective, genericRules bool) (*corev1.ConfigMap, error) {
	objective, err := kubeObjective.Internal()
	if err != nil {
		return nil, fmt.Errorf("failed to get objective: %w", err)
	}

	increases, err := objective.IncreaseRules()
	if err != nil {
		return nil, fmt.Errorf("failed to get increase rules: %w", err)
	}
	burnrates, err := objective.Burnrates()
	if err != nil {
		return nil, fmt.Errorf("failed to get burn rate rules: %w", err)
	}

	rule := monitoringv1.PrometheusRuleSpec{
		Groups: []monitoringv1.RuleGroup{increases, burnrates},
	}

	if genericRules {
		rules, err := objective.GenericRules()
		if err != nil {
			if err != slo.ErrGroupingUnsupported {
				return nil, fmt.Errorf("failed to get generic rules: %w", err)
			}
			// ignore these rules
		} else {
			rule.Groups = append(rule.Groups, rules)
		}
	}

	bytes, err := yaml.Marshal(rule)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal recording rule: %w", err)
	}

	data := map[string]string{
		fmt.Sprintf("%s.rules.yaml", name): string(bytes),
	}

	isController := true
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: kubeObjective.GetNamespace(),
			Labels:    kubeObjective.GetLabels(),
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: kubeObjective.APIVersion,
					Kind:       kubeObjective.Kind,
					Name:       kubeObjective.Name,
					UID:        kubeObjective.UID,
					Controller: &isController,
				},
			},
		},
		Data: data,
	}, nil
}

func makeMimirRuleGroup(kubeObjective pyrrav1alpha1.ServiceLevelObjective, genericRules bool, writeAlertingRules bool) (*rwrulefmt.RuleGroup, error) {
	objective, err := kubeObjective.Internal()
	if err != nil {
		return nil, fmt.Errorf("failed to get objective: %w", err)
	}

	increases, err := objective.IncreaseRules()
	if err != nil {
		return nil, fmt.Errorf("failed to get increase rules: %w", err)
	}
	increasesMimirRules := prometheusRulesToMimirRules(increases.Rules, writeAlertingRules)

	burnrates, err := objective.Burnrates()
	if err != nil {
		return nil, fmt.Errorf("failed to get burn rate rules: %w", err)
	}
	burnratesMimirRules := prometheusRulesToMimirRules(burnrates.Rules, writeAlertingRules)

	combinedRules := make([]rulefmt.RuleNode, len(increasesMimirRules)+len(burnratesMimirRules))
	i := 0
	for _, r := range increasesMimirRules {
		combinedRules[i] = r
		i++
	}
	for _, r := range burnratesMimirRules {
		combinedRules[i] = r
		i++
	}

	return &rwrulefmt.RuleGroup{
		RuleGroup: rulefmt.RuleGroup{
			Name:     kubeObjective.GetName(),
			Interval: model.Duration(time.Second * 30),
			Rules:    combinedRules,
		},
	}, nil
}

func makeGrafanaAlertRuleGroup(kubeObjective pyrrav1alpha1.ServiceLevelObjective, genericRules bool, writeAlertingRules bool) (*rwrulefmt.RuleGroup, error) {
	objective, err := kubeObjective.Internal()
	if err != nil {
		return nil, fmt.Errorf("failed to get objective: %w", err)
	}

	increases, err := objective.IncreaseRules()
	if err != nil {
		return nil, fmt.Errorf("failed to get increase rules: %w", err)
	}
	increasesAlertRuleGroup := prometheusRuleGroupToGrafanaAlertRuleGroup(increases)

	alertGroup := grafanav1beta1.GrafanaAlertRuleGroup{}
	return nil, nil
}

func prometheusRuleGroupToGrafanaAlertRuleGroup(prometheusRuleGroup monitoringv1.RuleGroup) grafanav1beta1.GrafanaAlertRuleGroup {
	rules := []grafanav1beta1.AlertRule{}

	for _, r := range prometheusRuleGroup.Rules {
		if r.Alert == "" {
			continue
		}

		rules = append(rules, prometheusRuleToGrafanaAlertRule(prometheusRuleGroup.Name, r))
	}

	// TODO add all fields
	return grafanav1beta1.GrafanaAlertRuleGroup{
		Spec: grafanav1beta1.GrafanaAlertRuleGroupSpec{
			Rules: rules,
		},
	}
}

func prometheusRuleToGrafanaAlertRule(sloName string, promRule monitoringv1.Rule) grafanav1beta1.AlertRule {
	forDuration := time.Minute * 5
	if promRule.For != nil {
		duration, err := time.ParseDuration(string(*promRule.For))
		if err != nil {
			forDuration = duration
		}
	}

	type Datasource struct {
		Type string `json:"type"`
		UID  string `json:"uid"`
	}

	type ModelA struct {
		Datasource    Datasource `json:"datasource"`
		EditorMode    string     `json:"editorMode"`
		Expr          string     `json:"expr"`
		Instant       bool       `json:"instant"`
		IntervalMs    int        `json:"intervalMs"`
		LegendFormat  string     `json:"legendFormat"`
		MaxDataPoints int        `json:"maxDataPoints"`
		Range         bool       `json:"range"`
		RefID         string     `json:"refId"`
	}

	type Evaluator struct {
		Params []int  `json:"params"`
		Type   string `json:"type"`
	}

	type Operator struct {
		Type string `json:"type"`
	}

	type Query struct {
		Params []string `json:"params"`
	}

	type Reducer struct {
		Params []interface{} `json:"params"`
		Type   string        `json:"type"`
	}

	type Condition struct {
		Evaluator Evaluator `json:"evaluator"`
		Operator  Operator  `json:"operator"`
		Query     Query     `json:"query"`
		Reducer   Reducer   `json:"reducer"`
		Type      string    `json:"type"`
	}

	type ModelB struct {
		DatasourceUID string      `json:"datasourceUid"`
		Conditions    []Condition `json:"conditions"`
		Datasource    Datasource  `json:"datasource"`
		Expression    string      `json:"expression"`
		IntervalMs    int         `json:"intervalMs"`
		MaxDataPoints int         `json:"maxDataPoints"`
		RefID         string      `json:"refId"`
		Type          string      `json:"type"`
	}
	//datasource:
	//type: {{ .datasourceType }}
	//uid: {{ .datasourceUID }}
	//editorMode: code
	//expr: {{ .expr }}
	//instant: true
	//intervalMs: 1000
	//legendFormat: __auto
	//maxDataPoints: 43200
	//range: false
	//refId: A

	modelA := ModelA{
		Datasource: Datasource{
			Type: "prometheus",
			UID:  "prometheus",
		},
		EditorMode:    "code",
		Expr:          promRule.Expr.String(),
		Instant:       true,
		IntervalMs:    1000,
		LegendFormat:  "__auto",
		MaxDataPoints: 43200,
		Range:         false,
		RefID:         "A",
	}

	//conditions:
	//- evaluator:
	//params:
	//- {{ .conditionThreshold }}
	//type: {{ .conditionType }}
	//operator:
	//type: and
	//query:
	//params:
	//- C
	//reducer:
	//params: []
	//type: last
	//type: query
	//datasource:
	//type: __expr__
	//uid: __expr__
	//expression: A
	//intervalMs: 1000
	//maxDataPoints: 43200
	//refId: B
	//type: threshold
	modelB := ModelB{
		DatasourceUID: "__expr__",
		Conditions: []Condition{
			Condition{}, // TODO try parse condition from prometheusrule or pass dirctly?
		},
		Datasource: Datasource{
			Type: "__expr__",
			UID:  "__expr__",
		},
		Expression:    "A",
		IntervalMs:    1000,
		MaxDataPoints: 43200,
		RefID:         "B",
		Type:          "threshold",
	}

	//TODO: check error?
	modelABytes, _ := json.Marshal(modelA)
	modelBBytes, _ := json.Marshal(modelB)

	return grafanav1beta1.AlertRule{
		UID:         grafanaAlertRuleUID(sloName, promRule.Alert),
		For:         &metav1.Duration{forDuration},
		Title:       promRule.Alert,
		Labels:      promRule.Labels,
		Annotations: promRule.Annotations,
		Condition:   "B",
		// TODO add actual model
		Data: []*grafanav1beta1.AlertQuery{
			&grafanav1beta1.AlertQuery{
				RefID: "A",
				// TODO: read from annotation or CRD?
				DatasourceUID: "prometheus",
				Model:         &v1.JSON{modelABytes},
			},
			&grafanav1beta1.AlertQuery{
				RefID:         "B",
				DatasourceUID: "__expr__",
				Model:         &v1.JSON{modelBBytes},
			},
		},
	}
}

func grafanaAlertRuleUID(sloName, ruleName string) string {
	return fmt.Sprintf("%s", sha1.Sum([]byte(fmt.Sprintf("%s-%s", sloName, ruleName))))
}

func prometheusRuleToMimirRuleNode(promRule monitoringv1.Rule) rulefmt.RuleNode {
	if promRule.Alert != "" {
		forVal := time.Minute * 5
		if promRule.For != nil {
			// TODO
			forVal, _ = time.ParseDuration(string(*promRule.For))
		}
		return rulefmt.RuleNode{
			Expr:        yamlStringNode(promRule.Expr.String()),
			Alert:       yamlStringNode(promRule.Alert),
			For:         model.Duration(forVal),
			Labels:      promRule.Labels,
			Annotations: promRule.Annotations,
		}
	}
	return rulefmt.RuleNode{
		Expr:   yamlStringNode(promRule.Expr.String()),
		Record: yamlStringNode(promRule.Record),
		Labels: promRule.Labels,
	}
}

func yamlStringNode(val string) yamlv3.Node {
	n := yamlv3.Node{}
	n.SetString(val)
	return n
}

func prometheusRulesToMimirRules(promRules []monitoringv1.Rule, writeAlertingRules bool) []rulefmt.RuleNode {
	rules := []rulefmt.RuleNode{}
	for _, r := range promRules {
		if r.Alert != "" && !writeAlertingRules {
			continue
		}
		rules = append(rules, prometheusRuleToMimirRuleNode(r))
	}

	return rules
}

func makePrometheusRule(kubeObjective pyrrav1alpha1.ServiceLevelObjective, genericRules bool) (*monitoringv1.PrometheusRule, error) {
	objective, err := kubeObjective.Internal()
	if err != nil {
		return nil, fmt.Errorf("failed to get objective: %w", err)
	}

	increases, err := objective.IncreaseRules()
	if err != nil {
		return nil, fmt.Errorf("failed to get increase rules: %w", err)
	}
	burnrates, err := objective.Burnrates()
	if err != nil {
		return nil, fmt.Errorf("failed to get burn rate rules: %w", err)
	}

	rule := monitoringv1.PrometheusRuleSpec{
		Groups: []monitoringv1.RuleGroup{increases, burnrates},
	}

	if genericRules {
		rules, err := objective.GenericRules()
		if err != nil {
			if err != slo.ErrGroupingUnsupported {
				return nil, fmt.Errorf("failed to get generic rules: %w", err)
			}
			// ignore these rules
		} else {
			rule.Groups = append(rule.Groups, rules)
		}
	}

	isController := true
	return &monitoringv1.PrometheusRule{
		TypeMeta: metav1.TypeMeta{
			Kind:       monitoringv1.PrometheusRuleKind,
			APIVersion: monitoring.GroupName + "/" + monitoringv1.Version,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeObjective.GetName(),
			Namespace: kubeObjective.GetNamespace(),
			Labels:    kubeObjective.GetLabels(),
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: kubeObjective.APIVersion,
					Kind:       kubeObjective.Kind,
					Name:       kubeObjective.Name,
					UID:        kubeObjective.UID,
					Controller: &isController,
				},
			},
		},
		Spec: rule,
	}, nil
}
