package postgres

import (
	"net/url"
	"os"
)

// DatabaseURLFromEnvironment accepts the existing development URL or assembles
// the cluster URL from a Secret-backed password environment variable.
func DatabaseURLFromEnvironment() string {
	if configured := os.Getenv("POLYGATE_DATABASE_URL"); configured != "" {
		return configured
	}
	password := os.Getenv("POLYGATE_DB_PASSWORD")
	if password == "" {
		return ""
	}
	host := os.Getenv("POLYGATE_DB_HOST")
	if host == "" {
		host = "postgres-client.polygate-system.svc.cluster.local:5432"
	}
	u := &url.URL{Scheme: "postgres", Host: host, Path: "/polygate", User: url.UserPassword("polygate", password)}
	query := u.Query()
	query.Set("sslmode", "disable")
	u.RawQuery = query.Encode()
	return u.String()
}
