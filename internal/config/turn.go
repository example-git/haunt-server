package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

type TURN struct {
	Enabled                                       bool
	PublicIP, PublicHost, ListenAddr, RelayBindIP string
	TLSAddr, TLSCert, TLSKey                      string
	RelayMin, RelayMax                            uint16
	CredentialTTL                                 time.Duration
	MaxAllocations                                int
	AllowedPeers                                  []netip.Prefix
}

func loadTURN(c *Config) error {
	raw, exists := os.LookupEnv("HAUNT_RELAY_ENABLED")
	if !exists {
		return nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return fmt.Errorf("HAUNT_RELAY_ENABLED must be true or false")
	}
	c.TURN.Enabled = enabled
	if !enabled {
		return nil
	}
	v := &c.TURN
	if v.PublicIP, err = required("HAUNT_RELAY_PUBLIC_IP"); err != nil {
		return err
	}
	ip := net.ParseIP(v.PublicIP)
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("HAUNT_RELAY_PUBLIC_IP must be an advertised unicast IP address")
	}
	if v.PublicHost, err = textDefault("HAUNT_RELAY_PUBLIC_HOST", v.PublicIP); err != nil {
		return err
	}
	if strings.ContainsAny(v.PublicHost, "/ @?#\r\n\t") || (strings.Contains(v.PublicHost, ":") && net.ParseIP(v.PublicHost) == nil) {
		return fmt.Errorf("HAUNT_RELAY_PUBLIC_HOST must be a hostname or IP without a port")
	}
	if v.ListenAddr, err = textDefault("HAUNT_RELAY_LISTEN_ADDR", "0.0.0.0:3478"); err != nil {
		return err
	}
	if err = turnListen(v.ListenAddr); err != nil {
		return err
	}
	if v.RelayBindIP, err = textDefault("HAUNT_RELAY_BIND_IP", "0.0.0.0"); err != nil {
		return err
	}
	if bind := net.ParseIP(v.RelayBindIP); bind == nil || (bind.To4() == nil) != (ip.To4() == nil) {
		return fmt.Errorf("TURN relay bind/public IPs must use the same address family")
	}
	low, err := number("HAUNT_RELAY_PORT_MIN", 49160, 1, 65534)
	if err != nil {
		return err
	}
	high, err := number("HAUNT_RELAY_PORT_MAX", 49999, 1, 65534)
	if err != nil {
		return err
	}
	if low > high {
		return fmt.Errorf("TURN relay maximum must be >= minimum")
	}
	v.RelayMin = uint16(low)
	v.RelayMax = uint16(high)
	ttl, err := number("HAUNT_RELAY_CREDENTIAL_MINUTES", 30, 1, 1440)
	if err != nil {
		return err
	}
	v.CredentialTTL = time.Duration(ttl) * time.Minute
	max, err := number("HAUNT_RELAY_MAX_ALLOCATIONS", 512, 1, 4096)
	if err != nil {
		return err
	}
	v.MaxAllocations = int(max)
	if raw, present := os.LookupEnv("HAUNT_RELAY_ALLOWED_PEER_CIDRS"); present {
		for _, entry := range strings.Split(raw, ",") {
			prefix, err := netip.ParsePrefix(strings.TrimSpace(entry))
			if err != nil || prefix.Bits() == 0 || len(v.AllowedPeers) >= 32 {
				return fmt.Errorf("TURN allowed peers require at most 32 specific CIDRs")
			}
			v.AllowedPeers = append(v.AllowedPeers, prefix.Masked())
		}
	}
	v.TLSAddr = os.Getenv("HAUNT_RELAY_TLS_ADDR")
	v.TLSCert = os.Getenv("HAUNT_RELAY_TLS_CERT")
	v.TLSKey = os.Getenv("HAUNT_RELAY_TLS_KEY")
	if v.TLSAddr != "" || v.TLSCert != "" || v.TLSKey != "" {
		if v.TLSAddr == "" || v.TLSCert == "" || v.TLSKey == "" {
			return fmt.Errorf("TURN TLS requires address, certificate, and private key")
		}
		if err = turnListen(v.TLSAddr); err != nil {
			return err
		}
	}
	return nil
}
func turnListen(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("TURN listener must be an IP:port address")
	}
	n, err := strconv.Atoi(port)
	if net.ParseIP(host) == nil || err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("TURN listener requires an IP and port 1-65535")
	}
	return nil
}
