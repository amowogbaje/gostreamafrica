package config

import "testing"

func TestLoad_Defaults(t *testing.T) {
	c := Load(func(string) string { return "" })
	if c.HealthAddr != ":8081" || c.Env != "local" || c.LogLevel != "info" {
		t.Fatalf("bad defaults: %+v", c)
	}
}

func TestLoad_Override(t *testing.T) {
	c := Load(func(k string) string {
		if k == "WORKER_HEALTH_ADDR" {
			return " :9999 "
		}
		return ""
	})
	if c.HealthAddr != ":9999" {
		t.Fatalf("got %q", c.HealthAddr)
	}
}
