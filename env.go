package main

import (
	"os"
	"strings"
)

// Env carries the cluster connection coordinates pulled from the
// process environment. Read once at process start; never mutated.
type Env struct {
	EtcdEndpoints []string
	S3Endpoint    string
	S3AccessKey   string
	S3SecretKey   string
	S3Bucket      string
}

// lookup returns the value of the first env var that's set and
// non-empty. OGOROD_ names come first so a user with both a
// pre-existing AWS/etcdctl environment and ogorod-specific
// overrides sees the overrides win.
func lookup(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}

	return ""
}

func loadEnv() Env {
	// Per-field: primary OGOROD_* name first, then AWS/etcdctl
	// conventional fallbacks. We resolve every field up-front then
	// report every missing one together — one validation error pass
	// beats the "fix, rerun, see next error, repeat" chain.
	fields := [][]string{
		{"OGOROD_ETCD_ENDPOINTS", "ETCDCTL_ENDPOINTS"},
		{"OGOROD_S3_ENDPOINT", "AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL"},
		{"OGOROD_S3_ACCESS_KEY", "AWS_ACCESS_KEY_ID"},
		{"OGOROD_S3_SECRET_KEY", "AWS_SECRET_ACCESS_KEY"},
		{"OGOROD_S3_BUCKET"},
	}

	values := make([]string, len(fields))
	var missing []string

	for i, names := range fields {
		values[i] = lookup(names...)

		if values[i] == "" {
			missing = append(missing, "  "+strings.Join(names, " | "))
		}
	}

	if len(missing) > 0 {
		ThrowFmt("missing env config; set at least one variable per line:\n%s", strings.Join(missing, "\n"))
	}

	e := Env{
		S3Endpoint:  values[1],
		S3AccessKey: values[2],
		S3SecretKey: values[3],
		S3Bucket:    values[4],
	}

	for _, x := range strings.Split(values[0], ",") {
		if x = strings.TrimSpace(x); x != "" {
			e.EtcdEndpoints = append(e.EtcdEndpoints, x)
		}
	}

	return e
}
