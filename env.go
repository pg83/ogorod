package main

import (
	"net/url"
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

// findMCHost scans the environment for any MC_HOST_<alias>=... entry
// and returns the first one. minio-client uses this format to carry
// a full endpoint+access+secret in a single variable, so when it's
// set we can skip the three individual OGOROD_S3_* vars.
func findMCHost() string {
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MC_HOST_") {
			continue
		}

		_, v, _ := strings.Cut(kv, "=")

		if v != "" {
			return v
		}
	}

	return ""
}

// parseS3URL splits a combined http[s]://access:secret@host[:port]
// URL (used by MC_HOST_* and OGOROD_S3_URL) into its three parts.
// Returns zeros if anything critical is missing.
func parseS3URL(raw string) (endpoint, access, secret string) {
	if raw == "" {
		return "", "", ""
	}

	u, err := url.Parse(raw)

	if err != nil || u.User == nil {
		return "", "", ""
	}

	access = u.User.Username()
	secret, _ = u.User.Password()

	if access == "" || secret == "" {
		return "", "", ""
	}

	endpoint = u.Scheme + "://" + u.Host

	return endpoint, access, secret
}

func loadEnv() Env {
	// Compound-URL fallback: OGOROD_S3_URL or MC_HOST_<alias> carry
	// endpoint+access+secret together. Used as the "middle tier" —
	// individual OGOROD_S3_{ENDPOINT,ACCESS_KEY,SECRET_KEY} still
	// override per-field, AWS_* fall through underneath.
	compoundEndpoint, compoundAccess, compoundSecret := parseS3URL(
		lookup("OGOROD_S3_URL"),
	)

	if compoundEndpoint == "" {
		compoundEndpoint, compoundAccess, compoundSecret = parseS3URL(findMCHost())
	}

	// Per-field lookup with the compound tier inserted between
	// OGOROD_* and AWS_*. orElse picks the first non-empty.
	orElse := func(primary, mid, fallback string) string {
		if primary != "" {
			return primary
		}

		if mid != "" {
			return mid
		}

		return fallback
	}

	etcd := lookup("OGOROD_ETCD_ENDPOINTS", "ETCDCTL_ENDPOINTS")
	endpoint := orElse(lookup("OGOROD_S3_ENDPOINT"), compoundEndpoint, lookup("AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL"))
	access := orElse(lookup("OGOROD_S3_ACCESS_KEY"), compoundAccess, lookup("AWS_ACCESS_KEY_ID"))
	secret := orElse(lookup("OGOROD_S3_SECRET_KEY"), compoundSecret, lookup("AWS_SECRET_ACCESS_KEY"))
	bucket := lookup("OGOROD_S3_BUCKET")

	// Collect every missing field for a single diagnostic. Each
	// line shows the full list of names that would fill that field,
	// so the user can pick whichever family they already have.
	type row struct {
		val   string
		names string
	}
	rows := []row{
		{etcd, "OGOROD_ETCD_ENDPOINTS | ETCDCTL_ENDPOINTS"},
		{endpoint, "OGOROD_S3_ENDPOINT | OGOROD_S3_URL | MC_HOST_<alias> | AWS_ENDPOINT_URL_S3 | AWS_ENDPOINT_URL"},
		{access, "OGOROD_S3_ACCESS_KEY | OGOROD_S3_URL | MC_HOST_<alias> | AWS_ACCESS_KEY_ID"},
		{secret, "OGOROD_S3_SECRET_KEY | OGOROD_S3_URL | MC_HOST_<alias> | AWS_SECRET_ACCESS_KEY"},
		{bucket, "OGOROD_S3_BUCKET"},
	}

	var missing []string
	for _, r := range rows {
		if r.val == "" {
			missing = append(missing, "  "+r.names)
		}
	}

	if len(missing) > 0 {
		ThrowFmt("missing env config; set at least one variable per line:\n%s", strings.Join(missing, "\n"))
	}

	e := Env{
		S3Endpoint:  endpoint,
		S3AccessKey: access,
		S3SecretKey: secret,
		S3Bucket:    bucket,
	}

	for _, x := range strings.Split(etcd, ",") {
		if x = strings.TrimSpace(x); x != "" {
			e.EtcdEndpoints = append(e.EtcdEndpoints, x)
		}
	}

	return e
}
