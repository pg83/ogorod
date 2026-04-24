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

func loadEnv() Env {
	e := Env{
		S3Endpoint:  os.Getenv("OGOROD_S3_ENDPOINT"),
		S3AccessKey: os.Getenv("OGOROD_S3_ACCESS_KEY"),
		S3SecretKey: os.Getenv("OGOROD_S3_SECRET_KEY"),
		S3Bucket:    os.Getenv("OGOROD_S3_BUCKET"),
	}

	eps := os.Getenv("OGOROD_ETCD_ENDPOINTS")

	if eps == "" {
		ThrowFmt("OGOROD_ETCD_ENDPOINTS is required (comma-separated host:port)")
	}

	for _, x := range strings.Split(eps, ",") {
		if x = strings.TrimSpace(x); x != "" {
			e.EtcdEndpoints = append(e.EtcdEndpoints, x)
		}
	}

	require := func(name, value string) {
		if value == "" {
			ThrowFmt("%s is required", name)
		}
	}

	require("OGOROD_S3_ENDPOINT", e.S3Endpoint)
	require("OGOROD_S3_ACCESS_KEY", e.S3AccessKey)
	require("OGOROD_S3_SECRET_KEY", e.S3SecretKey)
	require("OGOROD_S3_BUCKET", e.S3Bucket)

	return e
}
