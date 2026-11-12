package provider

import (
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"os"
	"path/filepath"
)

func writeProviderStats(dir string, stream *claudeStream) {
	if dir == "" || len(stream.results) != 1 {
		return
	}
	r := stream.results[0]
	sid, subtype := r.SessionID, r.Subtype
	v := domain.ProviderSessionStats{SessionID: &sid, Subtype: &subtype, IsError: r.ReportedError, NumTurns: r.NumTurns, DurationMS: r.DurationMS, CostUSD: r.TotalCostUSD}
	if sid == "" {
		v.SessionID = nil
	}
	if subtype == "" {
		v.Subtype = nil
	}
	raw, e := json.Marshal(v)
	if e != nil {
		return
	}
	f, e := os.OpenFile(filepath.Join(dir, "turn-stats.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(raw)
}
