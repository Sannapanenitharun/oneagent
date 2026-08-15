package collector

import (
	"log"
	"time"
)

// pollLogger reports failures from the interval-polled cloud collectors without
// falling into either of the two obvious traps.
//
// These collectors previously discarded every error (`_ = err`), so a wrong
// region, an expired credential, or a missing IAM permission was
// indistinguishable from "this metric genuinely has no data right now": the
// agent simply reported nothing, forever, with no way to tell why.
//
// Logging every failure is the opposite failure. At a 15s interval one
// misconfigured collector writes ~5,700 identical lines a day and buries
// everything else in the journal. So: the first failure is logged immediately,
// subsequent ones are counted and summarised at most once per pollReportEvery,
// and recovery is logged once so the log shows the problem ending as well as
// starting.
type pollLogger struct {
	name       string
	failing    bool
	lastReport time.Time
	suppressed int
}

const pollReportEvery = 5 * time.Minute

func (p *pollLogger) fail(err error) {
	now := time.Now()
	if !p.failing {
		p.failing = true
		p.lastReport = now
		p.suppressed = 0
		log.Printf("%s: poll failed: %v", p.name, err)
		return
	}
	p.suppressed++
	if now.Sub(p.lastReport) >= pollReportEvery {
		log.Printf("%s: still failing (%d further failures since last report), most recent: %v",
			p.name, p.suppressed, err)
		p.lastReport = now
		p.suppressed = 0
	}
}

func (p *pollLogger) ok() {
	if p.failing {
		log.Printf("%s: recovered", p.name)
		p.failing = false
		p.suppressed = 0
	}
}
