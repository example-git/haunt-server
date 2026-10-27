package config

import (
	"strings"
	"testing"
	"time"
)

func TestCapacityConfiguration(t *testing.T) {
	values := map[string]string{"HAUNT_INSTANCE_ID": "capacity-test", "HAUNT_PUBLIC_URL": "http://localhost:8080", "HAUNT_DATABASE_URL": "postgres://localhost/test"}
	c, err := LoadValues(values)
	if err != nil {
		t.Fatal(err)
	}
	if c.QueueWait() != time.Second || c.Capacity[History].Active != 128 || c.Capacity[History].Queued != 128 {
		t.Fatal(c)
	}
	connections := int32(0)
	for _, limits := range c.Capacity {
		connections += limits.Connections
	}
	if connections != 38 {
		t.Fatal(connections)
	}
	for workload := range c.Capacity {
		prefix := strings.ToUpper(string(workload))
		for key, invalid := range map[string][]string{
			"HAUNT_HTTP_" + prefix + "_ACTIVE": {"", "0", "4097", "invalid"},
			"HAUNT_HTTP_" + prefix + "_QUEUED": {"", "-1", "8193"},
		} {
			for _, value := range invalid {
				values[key] = value
				if _, err := LoadValues(values); err == nil {
					t.Fatal(key, value)
				}
				delete(values, key)
			}
		}
		if workload != Control {
			key := "HAUNT_DB_" + prefix + "_CONNS"
			values[key] = "0"
			if _, err := LoadValues(values); err == nil {
				t.Fatal(key)
			}
			delete(values, key)
		}
	}
	values["HAUNT_HTTP_HISTORY_QUEUED"] = "0"
	values["HAUNT_DB_HISTORY_CONNS"] = "3"
	values["HAUNT_HTTP_QUEUE_WAIT_MS"] = "250"
	c, err = LoadValues(values)
	if err != nil || c.Capacity[History].Queued != 0 || c.Capacity[History].Connections != 3 || c.QueueWait() != 250*time.Millisecond {
		t.Fatal(c, err)
	}
}
