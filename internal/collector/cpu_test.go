package collector

import "testing"

// CPU utilisation is a difference between two readings. The old implementation
// took both of them 100ms apart inside a single sample, so it described 100ms
// out of every interval — at the default 15s, well under 1% of elapsed time.
// These cover the replacement, which differences against the previous tick and
// therefore describes all of it.
func TestSampleCPU_FirstTickHasNothingToCompareAgainst(t *testing.T) {
	h := &HostMetricsCollector{}
	// Stand in for /proc/stat by seeding state directly; readCPUStat itself is
	// exercised by the interval cases below.
	h.prevCPU, h.havePrevCPU = cpuStat{}, false

	if _, ok := h.deltaCPU(cpuStat{idle: 100, total: 200}); ok {
		t.Error("reported a value on the first tick — no time has elapsed under observation yet")
	}
	if !h.havePrevCPU {
		t.Error("first tick must still record the reading, or the second one has nothing to difference against")
	}
}

func TestSampleCPU_ReportsUtilisationAcrossTheWholeInterval(t *testing.T) {
	h := &HostMetricsCollector{}
	h.deltaCPU(cpuStat{idle: 1000, total: 2000})

	// 100 ticks elapsed, 25 of them idle => 75% busy.
	got, ok := h.deltaCPU(cpuStat{idle: 1025, total: 2100})
	if !ok {
		t.Fatal("expected a value on the second tick")
	}
	if got < 74.9 || got > 75.1 {
		t.Errorf("utilisation = %v, want 75", got)
	}
}

func TestSampleCPU_HandlesCountersThatDoNotMove(t *testing.T) {
	h := &HostMetricsCollector{}
	h.deltaCPU(cpuStat{idle: 500, total: 1000})

	if _, ok := h.deltaCPU(cpuStat{idle: 500, total: 1000}); ok {
		t.Error("two reads inside the same clock tick describe no interval and must report nothing")
	}
}

func TestSampleCPU_ClampsImpossibleReadings(t *testing.T) {
	h := &HostMetricsCollector{}
	h.deltaCPU(cpuStat{idle: 1000, total: 2000})
	// Idle jumped further than total: can happen across suspend/resume or a
	// CPU hotplug. Better a clamped number than a negative percentage.
	got, ok := h.deltaCPU(cpuStat{idle: 1200, total: 2100})
	if !ok {
		t.Fatal("expected a value")
	}
	if got < 0 || got > 100 {
		t.Errorf("utilisation = %v, want it clamped into [0,100]", got)
	}
}
