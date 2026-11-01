package detect

import "context"

// listGroup lists group pgid from /proc.
func listGroup(ctx context.Context, pgid int) ([]Process, error) {
	return procGroup(ctx, "/proc", pgid)
}
