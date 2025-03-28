// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package endpoint

import (
	"maps"

	"github.com/cilium/cilium/api/v1/models"
	loaderMetrics "github.com/cilium/cilium/pkg/datapath/loader/metrics"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/metrics"
	"github.com/cilium/cilium/pkg/metrics/metric"
	"github.com/cilium/cilium/pkg/spanstat"
	"github.com/cilium/cilium/pkg/time"
)

var endpointPolicyStatus endpointPolicyStatusMap

func init() {
	endpointPolicyStatus = newEndpointPolicyStatusMap()
}

type statistics interface {
	GetMap() map[string]*spanstat.SpanStat
}

func sendMetrics(stats statistics, metric metric.Vec[metric.Observer]) {
	for scope, stat := range stats.GetMap() {
		// Skip scopes that have not been hit (zero duration), so the count in
		// the histogram accurately reflects the number of times each scope is
		// hit, and the distribution is not incorrectly skewed towards zero.
		if stat.SuccessTotal() != time.Duration(0) {
			metric.WithLabelValues(scope, "success").Observe(stat.SuccessTotal().Seconds())
		}
		if stat.FailureTotal() != time.Duration(0) {
			metric.WithLabelValues(scope, "failure").Observe(stat.FailureTotal().Seconds())
		}
	}
}

type regenerationStatistics struct {
	success                    bool
	endpointID                 uint16
	policyStatus               models.EndpointPolicyEnabled
	totalTime                  spanstat.SpanStat
	waitingForLock             spanstat.SpanStat
	waitingForPolicyRepository spanstat.SpanStat
	waitingForCTClean          spanstat.SpanStat
	policyCalculation          spanstat.SpanStat
	selectorPolicyCalculation  spanstat.SpanStat
	endpointPolicyCalculation  spanstat.SpanStat
	proxyConfiguration         spanstat.SpanStat
	proxyPolicyCalculation     spanstat.SpanStat
	proxyWaitForAck            spanstat.SpanStat
	datapathRealization        loaderMetrics.SpanStat
	mapSync                    spanstat.SpanStat
	prepareBuild               spanstat.SpanStat
}

// SendMetrics sends the regeneration statistics for this endpoint to
// Prometheus.
func (s *regenerationStatistics) SendMetrics() {
	endpointPolicyStatus.Update(s.endpointID, s.policyStatus)

	if !s.success {
		// Endpoint regeneration failed, increase on failed metrics
		metrics.EndpointRegenerationTotal.WithLabelValues(metrics.LabelValueOutcomeFail).Inc()
		return
	}

	metrics.EndpointRegenerationTotal.WithLabelValues(metrics.LabelValueOutcomeSuccess).Inc()

	sendMetrics(s, metrics.EndpointRegenerationTimeStats)
}

// GetMap returns a map which key is the stat name and the value is the stat
func (s *regenerationStatistics) GetMap() map[string]*spanstat.SpanStat {
	result := map[string]*spanstat.SpanStat{
		"waitingForLock":             &s.waitingForLock,
		"waitingForPolicyRepository": &s.waitingForPolicyRepository,
		"waitingForCTClean":          &s.waitingForCTClean,
		"policyCalculation":          &s.policyCalculation,
		"proxyConfiguration":         &s.proxyConfiguration,
		"selectorPolicyCalculation":  &s.selectorPolicyCalculation,
		"endpointPolicyCalculation":  &s.endpointPolicyCalculation,
		"proxyPolicyCalculation":     &s.proxyPolicyCalculation,
		"proxyWaitForAck":            &s.proxyWaitForAck,
		"mapSync":                    &s.mapSync,
		"prepareBuild":               &s.prepareBuild,
		"total":                      &s.totalTime,
	}
	maps.Copy(result, s.datapathRealization.GetMap())
	return result
}

// used by PolicyRepository.GetSelectorPolicy
func (s *regenerationStatistics) WaitingForPolicyRepository() *spanstat.SpanStat {
	return &s.waitingForPolicyRepository
}

// used by PolicyRepository.GetSelectorPolicy
func (s *regenerationStatistics) SelectorPolicyCalculation() *spanstat.SpanStat {
	return &s.selectorPolicyCalculation
}

// endpointPolicyStatusMap is a map to store the endpoint id and the policy
// enforcement status. It is used only to send metrics to prometheus.
type endpointPolicyStatusMap struct {
	mutex lock.Mutex
	m     map[uint16]models.EndpointPolicyEnabled
}

func newEndpointPolicyStatusMap() endpointPolicyStatusMap {
	return endpointPolicyStatusMap{m: make(map[uint16]models.EndpointPolicyEnabled)}
}

// Update adds or updates a new endpoint to the map and update the metrics
// related
func (epPolicyMaps *endpointPolicyStatusMap) Update(endpointID uint16, policyStatus models.EndpointPolicyEnabled) {
	epPolicyMaps.mutex.Lock()
	epPolicyMaps.m[endpointID] = policyStatus
	epPolicyMaps.mutex.Unlock()
	endpointPolicyStatus.UpdateMetrics()
}

// Remove deletes the given endpoint from the map and update the metrics
func (epPolicyMaps *endpointPolicyStatusMap) Remove(endpointID uint16) {
	epPolicyMaps.mutex.Lock()
	delete(epPolicyMaps.m, endpointID)
	epPolicyMaps.mutex.Unlock()
	epPolicyMaps.UpdateMetrics()
}

// UpdateMetrics update the policy enforcement metrics statistics for the endpoints.
func (epPolicyMaps *endpointPolicyStatusMap) UpdateMetrics() {
	policyStatus := map[models.EndpointPolicyEnabled]float64{
		models.EndpointPolicyEnabledNone:             0,
		models.EndpointPolicyEnabledEgress:           0,
		models.EndpointPolicyEnabledIngress:          0,
		models.EndpointPolicyEnabledBoth:             0,
		models.EndpointPolicyEnabledAuditDashEgress:  0,
		models.EndpointPolicyEnabledAuditDashIngress: 0,
		models.EndpointPolicyEnabledAuditDashBoth:    0,
	}

	epPolicyMaps.mutex.Lock()
	for _, value := range epPolicyMaps.m {
		policyStatus[value]++
	}
	epPolicyMaps.mutex.Unlock()

	for k, v := range policyStatus {
		metrics.PolicyEndpointStatus.WithLabelValues(string(k)).Set(v)
	}
}
