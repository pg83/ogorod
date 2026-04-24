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
	e := Env{
		// AWS_ENDPOINT_URL_S3 is the service-specific override;
		// AWS_ENDPOINT_URL is the global fallback. Both are
		// respected by aws-sdk-go-v2 itself, but we read them
		// directly so the diagnostic (which var to set) is crisp.
		S3Endpoint:  lookup("OGOROD_S3_ENDPOINT", "AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL"),
		S3AccessKey: lookup("OGOROD_S3_ACCESS_KEY", "AWS_ACCESS_KEY_ID"),
		S3SecretKey: lookup("OGOROD_S3_SECRET_KEY", "AWS_SECRET_ACCESS_KEY"),
		S3Bucket:    lookup("OGOROD_S3_BUCKET"),
	}

	// ETCDCTL_ENDPOINTS is already set on every lab host for
	// etcdctl lock/dedup scripts; reuse it.
	eps := lookup("OGOROD_ETCD_ENDPOINTS", "ETCDCTL_ENDPOINTS")

	if eps == "" {
		ThrowFmt("OGOROD_ETCD_ENDPOINTS or ETCDCTL_ENDPOINTS is required (comma-separated host:port)")
	}

	for _, x := range strings.Split(eps, ",") {
		if x = strings.TrimSpace(x); x != "" {
			e.EtcdEndpoints = append(e.EtcdEndpoints, x)
		}
	}

	require := func(value string, names ...string) {
		if value == "" {
			ThrowFmt("set one of: %s", strings.Join(names, ", "))
		}
	}

	require(e.S3Endpoint, "OGOROD_S3_ENDPOINT", "AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL")
	require(e.S3AccessKey, "OGOROD_S3_ACCESS_KEY", "AWS_ACCESS_KEY_ID")
	require(e.S3SecretKey, "OGOROD_S3_SECRET_KEY", "AWS_SECRET_ACCESS_KEY")
	require(e.S3Bucket, "OGOROD_S3_BUCKET")

	return e
}
