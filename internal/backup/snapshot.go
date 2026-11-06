package backup

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// snapshotMarker returns the path fragment shared by all snapshots of the partition date.
func snapshotMarker(date string) string {
	return "/partitions/" + date + "/snapshots/"
}

// toSlash converts Windows-style separators so that paths reported by
// VictoriaLogs running on any OS can be matched the same way.
func toSlash(p string) string {
	return strings.ReplaceAll(p, `\`, "/")
}

// selectSnapshot finds the snapshot created for the partition date.
//
// New snapshots are the paths present in after but not in before, plus the
// paths returned by the create call, restricted to snapshots of the given
// partition. When several candidates exist, the lexicographically greatest one
// (the newest, since snapshot names start with a timestamp) is selected and the
// remaining candidates are returned in others.
func selectSnapshot(before, after, created []string, date string) (selected string, others []string, err error) {
	marker := snapshotMarker(date)
	old := make(map[string]struct{}, len(before))
	for _, p := range before {
		old[p] = struct{}{}
	}
	seen := make(map[string]struct{})
	var candidates []string
	add := func(p string) {
		if !strings.Contains(toSlash(p), marker) {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		candidates = append(candidates, p)
	}
	for _, p := range after {
		if _, ok := old[p]; !ok {
			add(p)
		}
	}
	for _, p := range created {
		add(p)
	}
	if len(candidates) == 0 {
		return "", nil, fmt.Errorf("%w for partition %s; make sure the partition exists and is attached in VictoriaLogs "+
			"(snapshots before: %q; created: %q; after: %q)", ErrNoSnapshot, date, before, created, after)
	}
	slices.Sort(candidates)
	return candidates[len(candidates)-1], candidates[:len(candidates)-1], nil
}

// localSnapshotDir maps a snapshot path reported by VictoriaLogs (which may run
// in a container with a different mount point) to a local directory under storageDir.
//
// The part of vlPath starting at the last "/partitions/" must look exactly like
// partitions/<date>/snapshots/<name>. It returns the local directory and the snapshot name.
func localSnapshotDir(storageDir, vlPath, date string) (dir, name string, err error) {
	p := toSlash(vlPath)
	i := strings.LastIndex(p, "/partitions/")
	if i < 0 {
		return "", "", fmt.Errorf("snapshot path %q does not contain /partitions/", vlPath)
	}
	rel := strings.TrimSuffix(p[i+1:], "/")
	parts := strings.Split(rel, "/")
	if len(parts) != 4 || parts[0] != "partitions" || parts[1] != date || parts[2] != "snapshots" {
		return "", "", fmt.Errorf("snapshot path %q does not match .../partitions/%s/snapshots/<name>", vlPath, date)
	}
	name = parts[3]
	if name == "" || name == "." || name == ".." {
		return "", "", fmt.Errorf("invalid snapshot name in path %q", vlPath)
	}
	return filepath.Join(storageDir, "partitions", date, "snapshots", name), name, nil
}
