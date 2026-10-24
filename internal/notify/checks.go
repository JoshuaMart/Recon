package notify

import (
	"context"
	"fmt"
	"net/http"
	"syscall"
	"time"

	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// HTTPCheck is a component answering 2xx on a health URL.
func HTTPCheck(component, url string) Check {
	return Check{Component: component, Probe: func(ctx context.Context) (bool, string) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, "bad health URL"
		}
		res, err := client.Do(req) //nolint:gosec // a URL from the config
		if err != nil {
			return false, "unreachable"
		}
		_ = res.Body.Close()
		if res.StatusCode >= 300 {
			return false, fmt.Sprintf("health answered %d", res.StatusCode)
		}
		return true, ""
	}}
}

// SyncCheck is the scope sync: failing when its last error is newer than its
// last success, a refused suspicious pass included.
func SyncCheck(q *sqlcgen.Queries) Check {
	return Check{Component: "scope sync", Probe: func(ctx context.Context) (bool, string) {
		st, err := q.GetSyncState(ctx)
		if err != nil {
			return true, "" // no pass yet: nothing to say
		}
		if st.LastErrorAt != nil && (st.LastSuccessAt == nil || st.LastErrorAt.After(*st.LastSuccessAt)) {
			detail := "last pass failed"
			if st.LastError != nil {
				detail = *st.LastError
			}
			return false, detail
		}
		return true, ""
	}}
}

// DiskCheck is the filesystem holding dir, failing past threshold in use.
func DiskCheck(component, dir string, threshold float64) Check {
	return Check{Component: component, Probe: func(context.Context) (bool, string) {
		used, err := DiskUsed(dir)
		if err != nil {
			return false, "unreadable: " + err.Error()
		}
		if used > threshold {
			return false, fmt.Sprintf("%.0f%% used, past %.0f%%", used*100, threshold*100)
		}
		return true, ""
	}}
}

// DiskUsed is the share of the filesystem holding dir that is in use.
func DiskUsed(dir string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	total := uint64(st.Blocks) * uint64(st.Bsize) //nolint:gosec,unconvert // field types differ per platform
	if total == 0 {
		return 0, nil
	}
	free := uint64(st.Bavail) * uint64(st.Bsize) //nolint:gosec,unconvert // field types differ per platform
	return 1 - float64(free)/float64(total), nil
}
