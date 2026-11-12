package runtime

import (
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"
)

func readProviderStats(dir string) *domain.ProviderSessionStats {
	f, e := os.OpenFile(filepath.Join(dir, "turn-stats.json"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > 8192 {
		return nil
	}
	var s domain.ProviderSessionStats
	if json.NewDecoder(io.LimitReader(f, 8192)).Decode(&s) != nil {
		return nil
	}
	if s.CostUSD != nil && (math.IsNaN(*s.CostUSD) || math.IsInf(*s.CostUSD, 0)) {
		s.CostUSD = nil
	}
	return &s
}
