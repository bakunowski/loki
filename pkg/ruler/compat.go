package ruler

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/sigv4"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/rulefmt"
	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/prometheus/prometheus/notifier"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/rules"
	"github.com/prometheus/prometheus/template"
	"github.com/weaveworks/common/user"
	"gopkg.in/yaml.v3"

	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/logql"
	"github.com/grafana/loki/pkg/logql/syntax"
	ruler "github.com/grafana/loki/pkg/ruler/base"
	"github.com/grafana/loki/pkg/ruler/rulespb"
	"github.com/grafana/loki/pkg/ruler/util"
)

// RulesLimits is the one function we need from limits.Overrides, and
// is here to limit coupling.
type RulesLimits interface {
	ruler.RulesLimits

	RulerRemoteWriteDisabled(userID string) bool
	RulerRemoteWriteURL(userID string) string
	RulerRemoteWriteTimeout(userID string) time.Duration
	RulerRemoteWriteHeaders(userID string) map[string]string
	RulerRemoteWriteRelabelConfigs(userID string) []*util.RelabelConfig
	RulerRemoteWriteQueueCapacity(userID string) int
	RulerRemoteWriteQueueMinShards(userID string) int
	RulerRemoteWriteQueueMaxShards(userID string) int
	RulerRemoteWriteQueueMaxSamplesPerSend(userID string) int
	RulerRemoteWriteQueueBatchSendDeadline(userID string) time.Duration
	RulerRemoteWriteQueueMinBackoff(userID string) time.Duration
	RulerRemoteWriteQueueMaxBackoff(userID string) time.Duration
	RulerRemoteWriteQueueRetryOnRateLimit(userID string) bool
	RulerRemoteWriteSigV4Config(userID string) *sigv4.SigV4Config
}

// engineQueryFunc returns a new query function using the rules.EngineQueryFunc function
// and passing an altered timestamp.
func engineQueryFunc(engine *logql.Engine, overrides RulesLimits, checker readyChecker, userID string) rules.QueryFunc {
	return rules.QueryFunc(func(ctx context.Context, qs string, t time.Time) (promql.Vector, error) {
		// check if storage instance is ready; if not, fail the rule evaluation;
		// we do this to prevent an attempt to append new samples before the WAL appender is ready
		if !checker.isReady(userID) {
			return nil, errNotReady
		}

		adjusted := t.Add(-overrides.EvaluationDelay(userID))
		params := logql.NewLiteralParams(
			qs,
			adjusted,
			adjusted,
			0,
			0,
			logproto.FORWARD,
			0,
			nil,
		)
		q := engine.Query(params)

		res, err := q.Exec(ctx)
		if err != nil {
			return nil, err
		}
		switch v := res.Data.(type) {
		case promql.Vector:
			return v, nil
		case promql.Scalar:
			return promql.Vector{promql.Sample{
				Point:  promql.Point(v),
				Metric: labels.Labels{},
			}}, nil
		default:
			return nil, errors.New("rule result is not a vector or scalar")
		}
	})
}

// MultiTenantManagerAdapter will wrap a MultiTenantManager which validates loki rules
func MultiTenantManagerAdapter(mgr ruler.MultiTenantManager) ruler.MultiTenantManager {
	return &MultiTenantManager{inner: mgr}
}

// MultiTenantManager wraps a cortex MultiTenantManager but validates loki rules
type MultiTenantManager struct {
	inner ruler.MultiTenantManager
}

func (m *MultiTenantManager) SyncRuleGroups(ctx context.Context, ruleGroups map[string]rulespb.RuleGroupList) {
	m.inner.SyncRuleGroups(ctx, ruleGroups)
}

func (m *MultiTenantManager) GetRules(userID string) []*rules.Group {
	return m.inner.GetRules(userID)
}

func (m *MultiTenantManager) Stop() {
	if registry != nil {
		registry.stop()
	}

	m.inner.Stop()
}

// ValidateRuleGroup validates a rulegroup
func (m *MultiTenantManager) ValidateRuleGroup(grp rulefmt.RuleGroup) []error {
	return ValidateGroups(grp)
}

// MetricsPrefix defines the prefix to use for all metrics in this package
const MetricsPrefix = "loki_ruler_wal_"

var registry storageRegistry

func MultiTenantRuleManager(cfg Config, engine *logql.Engine, overrides RulesLimits, logger log.Logger, reg prometheus.Registerer) ruler.ManagerFactory {
	reg = prometheus.WrapRegistererWithPrefix(MetricsPrefix, reg)

	registry = newWALRegistry(log.With(logger, "storage", "registry"), reg, cfg, overrides)

	return func(
		ctx context.Context,
		userID string,
		notifier *notifier.Manager,
		logger log.Logger,
		reg prometheus.Registerer,
	) ruler.RulesManager {
		registry.configureTenantStorage(userID)

		logger = log.With(logger, "user", userID)
		queryFunc := engineQueryFunc(engine, overrides, registry, userID)
		memStore := NewMemStore(userID, queryFunc, newMemstoreMetrics(reg), 5*time.Minute, log.With(logger, "subcomponent", "MemStore"))

		mgr := rules.NewManager(&rules.ManagerOptions{
			Appendable:      registry,
			Queryable:       memStore,
			QueryFunc:       queryFunc,
			Context:         user.InjectOrgID(ctx, userID),
			ExternalURL:     cfg.ExternalURL.URL,
			NotifyFunc:      ruler.SendAlerts(notifier, cfg.ExternalURL.URL.String()),
			Logger:          logger,
			Registerer:      reg,
			OutageTolerance: cfg.OutageTolerance,
			ForGracePeriod:  cfg.ForGracePeriod,
			ResendDelay:     cfg.ResendDelay,
			GroupLoader:     GroupLoader{},
		})

		// initialize memStore, bound to the manager's alerting rules
		memStore.Start(mgr)

		return mgr
	}
}

type GroupLoader struct{}

func (GroupLoader) Parse(query string) (parser.Expr, error) {
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		return nil, err
	}

	return exprAdapter{expr}, nil
}

func (g GroupLoader) Load(identifier string) (*rulefmt.RuleGroups, []error) {
	b, err := ioutil.ReadFile(identifier)
	if err != nil {
		return nil, []error{errors.Wrap(err, identifier)}
	}
	rgs, errs := g.parseRules(b)
	for i := range errs {
		errs[i] = errors.Wrap(errs[i], identifier)
	}
	return rgs, errs
}

func (GroupLoader) parseRules(content []byte) (*rulefmt.RuleGroups, []error) {
	var (
		groups rulefmt.RuleGroups
		errs   []error
	)

	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)

	if err := decoder.Decode(&groups); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return nil, errs
	}

	return &groups, ValidateGroups(groups.Groups...)
}

func ValidateGroups(grps ...rulefmt.RuleGroup) (errs []error) {
	set := map[string]struct{}{}

	for i, g := range grps {
		if g.Name == "" {
			errs = append(errs, errors.Errorf("group %d: Groupname must not be empty", i))
		}

		if _, ok := set[g.Name]; ok {
			errs = append(
				errs,
				errors.Errorf("groupname: \"%s\" is repeated in the same file", g.Name),
			)
		}

		set[g.Name] = struct{}{}

		for _, r := range g.Rules {
			if err := validateRuleNode(&r, g.Name); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errs
}

func validateRuleNode(r *rulefmt.RuleNode, groupName string) error {
	if r.Record.Value != "" && r.Alert.Value != "" {
		return errors.Errorf("only one of 'record' and 'alert' must be set")
	}

	if r.Record.Value == "" && r.Alert.Value == "" {
		return errors.Errorf("one of 'record' or 'alert' must be set")
	}

	if r.Expr.Value == "" {
		return errors.Errorf("field 'expr' must be set in rule")
	} else if _, err := syntax.ParseExpr(r.Expr.Value); err != nil {
		return errors.Wrapf(err, fmt.Sprintf("could not parse expression for record '%s' in group '%s'", r.Record.Value, groupName))
	}

	if r.Record.Value != "" {
		if len(r.Annotations) > 0 {
			return errors.Errorf("invalid field 'annotations' in recording rule")
		}
		if r.For != 0 {
			return errors.Errorf("invalid field 'for' in recording rule")
		}
		if !model.IsValidMetricName(model.LabelValue(r.Record.Value)) {
			return errors.Errorf("invalid recording rule name: %s", r.Record.Value)
		}
	}

	for k, v := range r.Labels {
		if !model.LabelName(k).IsValid() || k == model.MetricNameLabel {
			return errors.Errorf("invalid label name: %s", k)
		}

		if !model.LabelValue(v).IsValid() {
			return errors.Errorf("invalid label value: %s", v)
		}
	}

	for k := range r.Annotations {
		if !model.LabelName(k).IsValid() {
			return errors.Errorf("invalid annotation name: %s", k)
		}
	}

	for _, err := range testTemplateParsing(r) {
		return err
	}

	return nil
}

// testTemplateParsing checks if the templates used in labels and annotations
// of the alerting rules are parsed correctly.
func testTemplateParsing(rl *rulefmt.RuleNode) (errs []error) {
	if rl.Alert.Value == "" {
		// Not an alerting rule.
		return errs
	}

	// Trying to parse templates.
	tmplData := template.AlertTemplateData(map[string]string{}, map[string]string{}, "", 0)
	defs := []string{
		"{{$labels := .Labels}}",
		"{{$externalLabels := .ExternalLabels}}",
		"{{$value := .Value}}",
	}
	parseTest := func(text string) error {
		tmpl := template.NewTemplateExpander(
			context.TODO(),
			strings.Join(append(defs, text), ""),
			"__alert_"+rl.Alert.Value,
			tmplData,
			model.Time(timestamp.FromTime(time.Now())),
			nil,
			nil,
			nil,
		)
		return tmpl.ParseTest()
	}

	// Parsing Labels.
	for k, val := range rl.Labels {
		err := parseTest(val)
		if err != nil {
			errs = append(errs, errors.Wrapf(err, "label %q", k))
		}
	}

	// Parsing Annotations.
	for k, val := range rl.Annotations {
		err := parseTest(val)
		if err != nil {
			errs = append(errs, errors.Wrapf(err, "annotation %q", k))
		}
	}

	return errs
}

// Allows logql expressions to be treated as promql expressions by the prometheus rules pkg.
type exprAdapter struct {
	syntax.Expr
}

func (exprAdapter) PositionRange() parser.PositionRange { return parser.PositionRange{} }
func (exprAdapter) PromQLExpr()                         {}
func (exprAdapter) Type() parser.ValueType              { return parser.ValueType("unimplemented") }
