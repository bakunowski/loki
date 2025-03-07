package gcplog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/relabel"
	"github.com/weaveworks/common/logging"
	"github.com/weaveworks/common/server"

	"github.com/grafana/loki/clients/pkg/promtail/api"
	"github.com/grafana/loki/clients/pkg/promtail/scrapeconfig"
	"github.com/grafana/loki/clients/pkg/promtail/targets/serverutils"
	"github.com/grafana/loki/clients/pkg/promtail/targets/target"

	util_log "github.com/grafana/loki/pkg/util/log"
)

type pushTarget struct {
	logger         log.Logger
	handler        api.EntryHandler
	config         *scrapeconfig.GcplogTargetConfig
	jobName        string
	server         *server.Server
	metrics        *Metrics
	relabelConfigs []*relabel.Config
}

// newPushTarget creates a brand new GCP Push target, capable of receiving message from a GCP PubSub push subscription.
func newPushTarget(metrics *Metrics, logger log.Logger, handler api.EntryHandler, jobName string, config *scrapeconfig.GcplogTargetConfig, relabel []*relabel.Config) (*pushTarget, error) {
	wrappedLogger := log.With(logger, "component", "gcp_push")

	ht := &pushTarget{
		metrics:        metrics,
		logger:         wrappedLogger,
		handler:        handler,
		jobName:        jobName,
		config:         config,
		relabelConfigs: relabel,
	}

	mergedServerConfigs, err := serverutils.MergeWithDefaults(config.Server)
	if err != nil {
		return nil, fmt.Errorf("failed to parse configs and override defaults when configuring gcp push target: %w", err)
	}
	config.Server = mergedServerConfigs

	err = ht.run()
	if err != nil {
		return nil, err
	}

	return ht, nil
}

func (h *pushTarget) run() error {
	level.Info(h.logger).Log("msg", "starting gcp push target", "job", h.jobName)

	// To prevent metric collisions because all metrics are going to be registered in the global Prometheus registry.

	tentativeServerMetricNamespace := "promtail_gcp_push_target_" + h.jobName
	if !model.IsValidMetricName(model.LabelValue(tentativeServerMetricNamespace)) {
		return fmt.Errorf("invalid prometheus-compatible job name: %s", h.jobName)
	}
	h.config.Server.MetricsNamespace = tentativeServerMetricNamespace

	// We don't want the /debug and /metrics endpoints running, since this is not the main promtail HTTP server.
	// We want this target to expose the least surface area possible, hence disabling WeaveWorks HTTP server metrics
	// and debugging functionality.
	h.config.Server.RegisterInstrumentation = false

	// Wrapping util logger with component-specific key vals, and the expected GoKit logging interface
	h.config.Server.Log = logging.GoKit(log.With(util_log.Logger, "component", "gcp_push"))

	srv, err := server.New(h.config.Server)
	if err != nil {
		return err
	}

	h.server = srv
	h.server.HTTP.Path("/gcp/api/v1/push").Methods("POST").Handler(http.HandlerFunc(h.push))

	go func() {
		err := srv.Run()
		if err != nil {
			level.Error(h.logger).Log("msg", "gcp push target shutdown with error", "err", err)
		}
	}()

	return nil
}

func (h *pushTarget) push(w http.ResponseWriter, r *http.Request) {
	entries := h.handler.Chan()
	defer r.Body.Close()

	pushMessage := PushMessage{}
	bs, err := io.ReadAll(r.Body)
	if err != nil {
		h.metrics.gcpPushErrors.WithLabelValues().Inc()
		level.Warn(h.logger).Log("msg", "failed to read incoming gcp push request", "err", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err = json.Unmarshal(bs, &pushMessage)
	if err != nil {
		h.metrics.gcpPushErrors.WithLabelValues().Inc()
		level.Warn(h.logger).Log("msg", "failed to unmarshall gcp push request", "err", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	entry, err := translate(pushMessage, h.config.Labels, h.config.UseIncomingTimestamp, h.relabelConfigs, r.Header.Get("X-Scope-OrgID"))
	if err != nil {
		h.metrics.gcpPushErrors.WithLabelValues().Inc()
		level.Warn(h.logger).Log("msg", "failed to translate gcp push request", "err", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	level.Debug(h.logger).Log("msg", fmt.Sprintf("Received line: %s", entry.Line))

	entries <- entry
	h.metrics.gcpPushEntries.WithLabelValues().Inc()
	w.WriteHeader(http.StatusNoContent)
}

func (h *pushTarget) Type() target.TargetType {
	return target.GcplogTargetType
}

func (h *pushTarget) DiscoveredLabels() model.LabelSet {
	return nil
}

func (h *pushTarget) Labels() model.LabelSet {
	return h.config.Labels
}

func (h *pushTarget) Ready() bool {
	return true
}

func (h *pushTarget) Details() interface{} {
	return map[string]string{}
}

func (h *pushTarget) Stop() error {
	level.Info(h.logger).Log("msg", "stopping gcp push target", "job", h.jobName)
	h.server.Shutdown()
	h.handler.Stop()
	return nil
}
