package builtin

import (
	"context"
	"fmt"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/tools"
)

// checkReadFreshness enforces the "read before mutate" contract: the
// caller must have opened the path via read_file during this session,
// and the file's on-disk size+mtime must still match what we saw at
// read time. Verb is just the action name used in the error message
// ("editing", "patching", "overwriting"). Returns nil when the executor
// does not provide a ReadTracker — third-party executors that don't
// opt in silently skip the check.
func checkReadFreshness(ctx context.Context, exec executor.Executor, path, verb string) error {
	tracker, ok := exec.(tools.ReadTracker)
	if !ok {
		return nil
	}
	snap, read := tracker.ReadSnapshot(path)
	if !read {
		return fmt.Errorf("must call read_file on %s before %s so the exact current contents are known", path, verb)
	}

	info, statErr := exec.Stat(ctx, path)
	if statErr != nil || info == nil {
		// File disappeared or can't be stat'd — treat as changed so we
		// don't blindly overwrite something unexpected.
		return fmt.Errorf("file %s is no longer accessible since it was read; call read_file again to refresh", path)
	}
	if info.Size != snap.Size || !info.ModTime.Equal(snap.Mtime) {
		return fmt.Errorf("file %s changed on disk since it was read (size %d→%d, mtime %s→%s); call read_file again before %s", path, snap.Size, info.Size, snap.Mtime.Format("15:04:05"), info.ModTime.Format("15:04:05"), verb)
	}
	return nil
}
