package config

import (
	"strings"
	"time"
)

type Workload string

const (
	Control     Workload = "control"
	History     Workload = "history"
	Messages    Workload = "messages"
	Embeds      Workload = "embeds"
	Media       Workload = "media"
	Realtime    Workload = "realtime"
	Maintenance Workload = "maintenance"
)

type WorkloadCapacity struct {
	Active      int
	Queued      int
	Connections int32
}

func (c Config) WorkloadCapacities() map[Workload]WorkloadCapacity {
	limits := map[Workload]WorkloadCapacity{
		Control:     {64, 64, c.DBMaxConns},
		History:     {128, 128, 8},
		Messages:    {32, 64, 4},
		Embeds:      {8, 32, 2},
		Media:       {32, 32, 2},
		Realtime:    {32, 32, 4},
		Maintenance: {1, 1, 2},
	}
	for workload, capacity := range c.Capacity {
		limits[workload] = capacity
	}
	return limits
}

func (c Config) QueueWait() time.Duration {
	if c.HTTPQueueWait == 0 {
		return time.Second
	}
	return c.HTTPQueueWait
}

func (e source) loadCapacity(c *Config) error {
	wait, err := e.number("HAUNT_HTTP_QUEUE_WAIT_MS", 1000, 1, 10000)
	if err != nil {
		return err
	}
	c.HTTPQueueWait = time.Duration(wait) * time.Millisecond
	c.Capacity = c.WorkloadCapacities()
	for workload, limits := range c.Capacity {
		prefix := "HAUNT_HTTP_" + strings.ToUpper(string(workload))
		active, err := e.number(prefix+"_ACTIVE", int64(limits.Active), 1, 4096)
		if err != nil {
			return err
		}
		queued, err := e.number(prefix+"_QUEUED", int64(limits.Queued), 0, 8192)
		if err != nil {
			return err
		}
		limits.Active, limits.Queued = int(active), int(queued)
		if workload != Control {
			connections, err := e.number("HAUNT_DB_"+strings.ToUpper(string(workload))+"_CONNS", int64(limits.Connections), 1, 1024)
			if err != nil {
				return err
			}
			limits.Connections = int32(connections)
		}
		c.Capacity[workload] = limits
	}
	return nil
}
