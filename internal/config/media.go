package config

import (
	"encoding/json"
	"fmt"
	"github.com/pion/stun/v4"
	"io"
	"net"
	"os"
	"strings"
)

func loadMedia(c *Config) error {
	if raw, present := os.LookupEnv("HAUNT_ICE_SERVERS"); present {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&c.ICE) != nil || c.ICE == nil || len(c.ICE) > 8 {
			return fmt.Errorf("HAUNT_ICE_SERVERS must be a JSON array of at most eight ICE servers")
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return fmt.Errorf("HAUNT_ICE_SERVERS must contain a single JSON value")
		}
		for _, server := range c.ICE {
			if len(server.URLs) < 1 || len(server.URLs) > 8 {
				return fmt.Errorf("each ICE server requires 1-8 URLs")
			}
			for _, url := range server.URLs {
				parsed, err := stun.ParseURI(url)
				if len(url) > 512 || strings.ContainsAny(url, "\r\n \t") || err != nil || parsed.Port < 1 || parsed.Port > 65535 {
					return fmt.Errorf("ICE URLs must use stun/stuns/turn/turns schemes")
				}
				if (parsed.Scheme == stun.SchemeTypeTURN || parsed.Scheme == stun.SchemeTypeTURNS) && (server.Username == "" || server.Credential == "") {
					return fmt.Errorf("TURN servers require a username and credential")
				}
			}
			if len(server.Username) > 512 || len(server.Credential) > 512 {
				return fmt.Errorf("ICE credentials too long")
			}
		}
	}
	if raw, present := os.LookupEnv("HAUNT_MEDIA_PUBLIC_IP"); present {
		if net.ParseIP(raw) == nil {
			return fmt.Errorf("HAUNT_MEDIA_PUBLIC_IP must be an IP address")
		}
		c.MediaPublicIP = raw
	}
	low, err := number("HAUNT_MEDIA_PORT_MIN", 0, 0, 65535)
	if err != nil {
		return err
	}
	high, err := number("HAUNT_MEDIA_PORT_MAX", 0, 0, 65535)
	if err != nil {
		return err
	}
	if (low == 0) != (high == 0) || high < low {
		return fmt.Errorf("media UDP port range requires both bounds, with maximum >= minimum")
	}
	c.MediaPortMin = uint16(low)
	c.MediaPortMax = uint16(high)
	peers, err := number("HAUNT_MEDIA_MAX_PEERS", 512, 2, 4096)
	if err != nil {
		return err
	}
	c.MediaMaxPeers = int(peers)
	members, err := number("HAUNT_MEDIA_ROOM_LIMIT", 16, 2, 64)
	if err != nil {
		return err
	}
	c.MediaRoomLimit = int(members)
	return nil
}
