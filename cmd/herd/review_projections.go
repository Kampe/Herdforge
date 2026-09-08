package main

import (
	"github.com/Kampe/Herdforge/pkg/review"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func reviewProjectionMaps(rows []review.LedgerRow) (latest, records map[reviewledger.ProjectionKey]reviewledger.LedgerRow) {
	latest = make(map[reviewledger.ProjectionKey]reviewledger.LedgerRow)
	records = make(map[reviewledger.ProjectionKey]reviewledger.LedgerRow)
	for _, row := range rows {
		rl := reviewledger.LedgerRow{
			Event: row.Event, SHA: row.SHA, Reviewer: row.Reviewer, Host: row.Host,
			Verdict: row.Verdict, RetryOf: row.RetryOf, Task: row.Task,
			BuilderFamily: row.BuilderFamily,
		}
		k := reviewledger.ProjectionOf(row.SHA, row.Reviewer, row.Host)
		switch row.Event {
		case string(reviewledger.EventRecord):
			records[k] = rl
		case string(reviewledger.EventVerdict):
			latest[k] = rl
		}
	}
	return latest, records
}

func liveHostVeto(latest, records map[reviewledger.ProjectionKey]reviewledger.LedgerRow, sha string) bool {
	retry := reviewledger.RetrySupersessionFromLatest(latest, records, sha)
	for k, v := range latest {
		if k.SHA != sha {
			continue
		}
		if v.Verdict != string(reviewledger.VerdictFAIL) && v.Verdict != string(reviewledger.VerdictBLOCKED) {
			continue
		}
		if retry[k] {
			continue
		}
		rec, ok := records[k]
		if !ok || rec.BuilderFamily == "" || !reviewledger.FamilyAllowlist[rec.BuilderFamily] {
			continue
		}
		return true
	}
	return false
}
