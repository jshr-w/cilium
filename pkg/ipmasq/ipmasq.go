// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package ipmasq

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/fsnotify/fsnotify"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/cilium/cilium/pkg/logging/logfields"
)

var (
	// The following reserved by RFCs IP addr ranges are used by
	// https://github.com/kubernetes-sigs/ip-masq-agent
	defaultNonMasqCIDRs = map[string]netip.Prefix{
		"10.0.0.0/8":      netip.MustParsePrefix("10.0.0.0/8"),
		"172.16.0.0/12":   netip.MustParsePrefix("172.16.0.0/12"),
		"192.168.0.0/16":  netip.MustParsePrefix("192.168.0.0/16"),
		"100.64.0.0/10":   netip.MustParsePrefix("100.64.0.0/10"),
		"192.0.0.0/24":    netip.MustParsePrefix("192.0.0.0/24"),
		"192.0.2.0/24":    netip.MustParsePrefix("192.0.2.0/24"),
		"192.88.99.0/24":  netip.MustParsePrefix("192.88.99.0/24"),
		"198.18.0.0/15":   netip.MustParsePrefix("198.18.0.0/15"),
		"198.51.100.0/24": netip.MustParsePrefix("198.51.100.0/24"),
		"203.0.113.0/24":  netip.MustParsePrefix("203.0.113.0/24"),
		"240.0.0.0/4":     netip.MustParsePrefix("240.0.0.0/4"),
	}
	linkLocalCIDRIPv4Str = "169.254.0.0/16"
	linkLocalCIDRIPv4    = netip.MustParsePrefix(linkLocalCIDRIPv4Str)
	linkLocalCIDRIPv6Str = "fe80::/10"
	linkLocalCIDRIPv6    = netip.MustParsePrefix(linkLocalCIDRIPv6Str)
)

// ipnet is a wrapper type for netip.Prefix to enable de-serialization
// of CIDRs
type Ipnet netip.Prefix

func (c *Ipnet) UnmarshalJSON(json []byte) error {
	str := string(json)

	if json[0] != '"' {
		return fmt.Errorf("Invalid CIDR: %s", str)
	}

	n, err := parseCIDR(strings.Trim(str, `"`))
	if err != nil {
		return err
	}

	*c = Ipnet(n)
	return nil
}

func parseCIDR(c string) (netip.Prefix, error) {
	n, err := netip.ParsePrefix(c)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("Invalid CIDR %q: %w", c, err)
	}
	return n.Masked(), nil
}

// config represents the ip-masq-agent configuration file encoded as YAML
type config struct {
	NonMasqCIDRs      []Ipnet `json:"nonMasqueradeCIDRs"`
	MasqLinkLocalIPv4 bool    `json:"masqLinkLocal"`
	MasqLinkLocalIPv6 bool    `json:"masqLinkLocalIPv6"`
}

// IPMasqMap is an interface describing methods for manipulating an ipmasq map
type IPMasqMap interface {
	Update(cidr netip.Prefix) error
	Delete(cidr netip.Prefix) error
	Dump() ([]netip.Prefix, error)
}

// IPMasqAgent represents a state of the ip-masq-agent
type IPMasqAgent struct {
	logger                 *slog.Logger
	configPath             string
	masqLinkLocalIPv4      bool
	masqLinkLocalIPv6      bool
	nonMasqCIDRsFromConfig map[string]netip.Prefix
	nonMasqCIDRsInMap      map[string]netip.Prefix
	ipMasqMap              IPMasqMap
	watcher                *fsnotify.Watcher
	stop                   chan struct{}
	handlerFinished        chan struct{}
}

func NewIPMasqAgent(logger *slog.Logger, configPath string, ipMasqMap IPMasqMap) *IPMasqAgent {
	a := &IPMasqAgent{
		logger:                 logger,
		configPath:             configPath,
		nonMasqCIDRsFromConfig: map[string]netip.Prefix{},
		nonMasqCIDRsInMap:      map[string]netip.Prefix{},
		ipMasqMap:              ipMasqMap,
	}

	return a
}

// Start starts the ip-masq-agent goroutine which tracks the config file and
// updates the BPF map accordingly.
func (a *IPMasqAgent) Start() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create fsnotify watcher: %w", err)
	}
	a.watcher = watcher

	configDir := filepath.Dir(a.configPath)
	// The directory of the config should exist at this time, otherwise
	// the watcher will fail to add
	if err := a.watcher.Add(configDir); err != nil {
		a.watcher.Close()
		return fmt.Errorf("failed to add %q dir to fsnotify watcher: %w", configDir, err)
	}

	if err := a.restore(); err != nil {
		a.logger.Warn("Failed to restore", logfields.Error, err)
	}
	if err := a.Update(); err != nil {
		a.logger.Warn("Failed to update", logfields.Error, err)
	}

	a.stop = make(chan struct{})
	a.handlerFinished = make(chan struct{})

	go func() {
		for {
			select {
			case event := <-a.watcher.Events:
				a.logger.Debug("Received fsnotify event", logfields.Event, event)

				switch {
				case event.Has(fsnotify.Create),
					event.Has(fsnotify.Write),
					event.Has(fsnotify.Chmod),
					event.Has(fsnotify.Remove),
					event.Has(fsnotify.Rename):
					if err := a.Update(); err != nil {
						a.logger.Warn("Failed to update", logfields.Error, err)
					}
				default:
					a.logger.Warn("Watcher received unknown event. Ignoring...", logfields.Event, event)
				}
			case err := <-a.watcher.Errors:
				a.logger.Warn("Watcher received an error", logfields.Error, err)
			case <-a.stop:
				a.logger.Info("Stopping ip-masq-agent")
				close(a.handlerFinished)
				return
			}
		}
	}()

	return nil
}

// Stop stops the ip-masq-agent goroutine and the watcher.
func (a *IPMasqAgent) Stop() {
	close(a.stop)
	<-a.handlerFinished
	a.watcher.Close()
}

// Update updates the ipmasq BPF map entries with ones from the config file.
func (a *IPMasqAgent) Update() error {
	isEmpty, err := a.readConfig()
	if err != nil {
		return err
	}

	// Set default nonMasq CIDRS if user hasn't specified any
	if isEmpty {
		maps.Copy(a.nonMasqCIDRsFromConfig, defaultNonMasqCIDRs)
	}

	if !a.masqLinkLocalIPv4 {
		a.nonMasqCIDRsFromConfig[linkLocalCIDRIPv4Str] = linkLocalCIDRIPv4
	}

	if !a.masqLinkLocalIPv6 {
		a.nonMasqCIDRsFromConfig[linkLocalCIDRIPv6Str] = linkLocalCIDRIPv6
	}

	for cidrStr, cidr := range a.nonMasqCIDRsFromConfig {
		if _, ok := a.nonMasqCIDRsInMap[cidrStr]; !ok {
			a.logger.Info("Adding CIDR", logfields.CIDR, cidrStr)
			a.ipMasqMap.Update(cidr)
			a.nonMasqCIDRsInMap[cidrStr] = cidr
		}
	}

	for cidrStr, cidr := range a.nonMasqCIDRsInMap {
		if _, ok := a.nonMasqCIDRsFromConfig[cidrStr]; !ok {
			a.logger.Info("Removing CIDR", logfields.CIDR, cidrStr)
			a.ipMasqMap.Delete(cidr)
			delete(a.nonMasqCIDRsInMap, cidrStr)
		}
	}

	return nil
}

// readConfig reads the config file and populates IPMasqAgent.nonMasqCIDRsFromConfig
// with the CIDRs from the file.
func (a *IPMasqAgent) readConfig() (bool, error) {
	var cfg config

	raw, err := os.ReadFile(a.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			a.logger.Info("Config file not found", logfields.Path, a.configPath)
			a.nonMasqCIDRsFromConfig = map[string]netip.Prefix{}
			a.masqLinkLocalIPv4 = false
			a.masqLinkLocalIPv6 = false
			return true, nil
		}
		return false, fmt.Errorf("Failed to read %s: %w", a.configPath, err)
	}

	if len(raw) == 0 {
		a.nonMasqCIDRsFromConfig = map[string]netip.Prefix{}
		a.masqLinkLocalIPv4 = false
		a.masqLinkLocalIPv6 = false
		return true, nil
	}

	jsonStr, err := yaml.ToJSON(raw)
	if err != nil {
		return false, fmt.Errorf("Failed to convert to json: %w", err)
	}

	if err := json.Unmarshal(jsonStr, &cfg); err != nil {
		return false, fmt.Errorf("Failed to de-serialize json: %w", err)
	}

	nonMasqCIDRs := map[string]netip.Prefix{}
	for _, cidr := range cfg.NonMasqCIDRs {
		n := netip.Prefix(cidr)
		nonMasqCIDRs[n.String()] = n
	}
	a.nonMasqCIDRsFromConfig = nonMasqCIDRs
	a.masqLinkLocalIPv4 = cfg.MasqLinkLocalIPv4
	a.masqLinkLocalIPv6 = cfg.MasqLinkLocalIPv6

	return false, nil
}

func (a *IPMasqAgent) NonMasqCIDRsFromConfig() []netip.Prefix {
	return slices.Collect(maps.Values(a.nonMasqCIDRsFromConfig))
}

// restore dumps the ipmasq BPF map and populates IPMasqAgent.nonMasqCIDRsInMap
// with the CIDRs from the map.
func (a *IPMasqAgent) restore() error {
	cidrsInMap, err := a.ipMasqMap.Dump()
	if err != nil {
		return fmt.Errorf("Failed to dump ip-masq-agent cidrs from map: %w", err)
	}

	cidrs := map[string]netip.Prefix{}
	for _, cidr := range cidrsInMap {
		cidrs[cidr.String()] = cidr
	}
	a.nonMasqCIDRsInMap = cidrs

	return nil
}
