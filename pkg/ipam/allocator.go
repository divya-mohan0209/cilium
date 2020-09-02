// Copyright 2016-2020 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ipam

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/cilium/cilium/pkg/ipam/types"
	"github.com/cilium/cilium/pkg/metrics"
	"github.com/cilium/cilium/pkg/uuid"

	"github.com/sirupsen/logrus"
)

const (
	metricAllocate = "allocate"
	metricRelease  = "release"
	familyIPv4     = "ipv4"
	familyIPv6     = "ipv6"
)

// Error definitions
var (
	// ErrIPv4Disabled is returned when IPv4 allocation is disabled
	ErrIPv4Disabled = errors.New("IPv4 allocation disabled")

	// ErrIPv6Disabled is returned when Ipv6 allocation is disabled
	ErrIPv6Disabled = errors.New("IPv6 allocation disabled")
)

func (ipam *IPAM) lookupIPsByOwner(owner string) (ips []net.IP) {
	ipam.allocatorMutex.RLock()
	defer ipam.allocatorMutex.RUnlock()

	for ip, o := range ipam.owner {
		if o == owner {
			if parsedIP := net.ParseIP(ip); parsedIP != nil {
				ips = append(ips, parsedIP)
			}
		}
	}

	return
}

// AllocateIP allocates a IP address.
func (ipam *IPAM) AllocateIP(ip net.IP, metadata types.Metadata) (err error) {
	needSyncUpstream := true
	return ipam.allocateIP(ip, needSyncUpstream, metadata)
}

// AllocateIPWithoutSyncUpstream allocates a IP address without syncing upstream.
func (ipam *IPAM) AllocateIPWithoutSyncUpstream(ip net.IP, metadata types.Metadata) (err error) {
	needSyncUpstream := false
	return ipam.allocateIP(ip, needSyncUpstream, metadata)
}

// AllocateIPString is identical to AllocateIP but takes a string
func (ipam *IPAM) AllocateIPString(ipAddr string, metadata types.Metadata) error {
	ip := net.ParseIP(ipAddr)
	if ip == nil {
		return fmt.Errorf("Invalid IP address: %s", ipAddr)
	}

	return ipam.AllocateIP(ip, metadata)
}

func (ipam *IPAM) allocateIP(ip net.IP, needSyncUpstream bool, metadata types.Metadata) (err error) {
	ipam.allocatorMutex.Lock()
	defer ipam.allocatorMutex.Unlock()

	if ipam.blacklist.Contains(ip) {
		err = fmt.Errorf("IP %s is blacklisted, owned by %s", ip.String(), metadata.Owner)
		return
	}

	family := familyIPv4
	if ip.To4() != nil {
		if ipam.IPv4Allocator == nil {
			err = ErrIPv4Disabled
			return
		}

		if needSyncUpstream {
			if _, err = ipam.IPv4Allocator.Allocate(ip, metadata); err != nil {
				return
			}
		} else {
			if _, err = ipam.IPv4Allocator.AllocateWithoutSyncUpstream(ip, metadata); err != nil {
				return
			}
		}
	} else {
		family = familyIPv6
		if ipam.IPv6Allocator == nil {
			err = ErrIPv6Disabled
			return
		}

		if needSyncUpstream {
			if _, err = ipam.IPv6Allocator.Allocate(ip, metadata); err != nil {
				return
			}
		} else {
			if _, err = ipam.IPv6Allocator.AllocateWithoutSyncUpstream(ip, metadata); err != nil {
				return
			}
		}
	}

	log.WithFields(logrus.Fields{
		"ip":    ip.String(),
		"owner": metadata.Owner,
	}).Debugf("Allocated specific IP")

	ipam.owner[ip.String()] = metadata.Owner
	metrics.IpamEvent.WithLabelValues(metricAllocate, family).Inc()
	return
}

func (ipam *IPAM) allocateNextFamily(family Family, needSyncUpstream bool, metadata types.Metadata) (result *AllocationResult, err error) {
	var allocator Allocator
	switch family {
	case IPv6:
		allocator = ipam.IPv6Allocator
	case IPv4:
		allocator = ipam.IPv4Allocator

	default:
		err = fmt.Errorf("unknown address \"%s\" family requested", family)
		return
	}

	if allocator == nil {
		err = fmt.Errorf("%s allocator not available", family)
		return
	}

	for {
		if needSyncUpstream {
			result, err = allocator.AllocateNext(metadata)
		} else {
			result, err = allocator.AllocateNextWithoutSyncUpstream(metadata)
		}
		if err != nil {
			return
		}

		if !ipam.blacklist.Contains(result.IP) {
			log.WithFields(logrus.Fields{
				"ip":    result.IP.String(),
				"owner": metadata.Owner,
			}).Debugf("Allocated random IP")
			ipam.owner[result.IP.String()] = metadata.Owner
			metrics.IpamEvent.WithLabelValues(metricAllocate, string(family)).Inc()
			return
		}

		// The allocated IP is blacklisted, do not use it. The
		// blacklisted IP is now allocated so it won't be allocated in
		// the next iteration.
		ipam.owner[result.IP.String()] = fmt.Sprintf("%s (blacklisted)", metadata.Owner)
	}
}

// AllocateNextFamily allocates the next IP of the requested address family
func (ipam *IPAM) AllocateNextFamily(family Family, metadata types.Metadata) (result *AllocationResult, err error) {
	ipam.allocatorMutex.Lock()
	defer ipam.allocatorMutex.Unlock()

	needSyncUpstream := true

	return ipam.allocateNextFamily(family, needSyncUpstream, metadata)
}

// AllocateNextFamilyWithoutSyncUpstream allocates the next IP of the requested address family
// without syncing upstream
func (ipam *IPAM) AllocateNextFamilyWithoutSyncUpstream(family Family, metadata types.Metadata) (result *AllocationResult, err error) {
	ipam.allocatorMutex.Lock()
	defer ipam.allocatorMutex.Unlock()

	needSyncUpstream := false

	return ipam.allocateNextFamily(family, needSyncUpstream, metadata)
}

// AllocateNext allocates the next available IPv4 and IPv6 address out of the
// configured address pool. If family is set to "ipv4" or "ipv6", then
// allocation is limited to the specified address family. If the pool has been
// drained of addresses, an error will be returned.
func (ipam *IPAM) AllocateNext(family string, metadata types.Metadata) (ipv4Result, ipv6Result *AllocationResult, err error) {
	if (family == "ipv6" || family == "") && ipam.IPv6Allocator != nil {
		ipv6Result, err = ipam.AllocateNextFamily(IPv6, metadata)
		if err != nil {
			return
		}

	}

	if (family == "ipv4" || family == "") && ipam.IPv4Allocator != nil {
		ipv4Result, err = ipam.AllocateNextFamily(IPv4, metadata)
		if err != nil {
			if ipv6Result != nil {
				ipam.ReleaseIP(ipv6Result.IP)
			}
			return
		}
	}

	return
}

// AllocateNextWithExpiration is identical to AllocateNext but registers an
// expiration timer as well. This is identical to using AllocateNext() in
// combination with StartExpirationTimer()
func (ipam *IPAM) AllocateNextWithExpiration(family string, timeout time.Duration, metadata types.Metadata) (ipv4Result, ipv6Result *AllocationResult, err error) {
	ipv4Result, ipv6Result, err = ipam.AllocateNext(family, metadata)
	if err != nil {
		return nil, nil, err
	}

	if timeout != time.Duration(0) {
		for _, result := range []*AllocationResult{ipv4Result, ipv6Result} {
			if result != nil {
				result.ExpirationUUID, err = ipam.StartExpirationTimer(result.IP, timeout)
				if err != nil {
					if ipv4Result != nil {
						ipam.ReleaseIP(ipv4Result.IP)
					}
					if ipv6Result != nil {
						ipam.ReleaseIP(ipv6Result.IP)
					}
					return
				}
			}
		}
	}

	return
}

func (ipam *IPAM) releaseIPLocked(ip net.IP) error {
	family := familyIPv4
	if ip.To4() != nil {
		if ipam.IPv4Allocator == nil {
			return ErrIPv4Disabled
		}

		if err := ipam.IPv4Allocator.Release(ip); err != nil {
			return err
		}
	} else {
		family = familyIPv6
		if ipam.IPv6Allocator == nil {
			return ErrIPv6Disabled
		}

		if err := ipam.IPv6Allocator.Release(ip); err != nil {
			return err
		}
	}

	owner := ipam.owner[ip.String()]
	log.WithFields(logrus.Fields{
		"ip":    ip.String(),
		"owner": owner,
	}).Debugf("Released IP")
	delete(ipam.owner, ip.String())
	delete(ipam.expirationTimers, ip.String())

	metrics.IpamEvent.WithLabelValues(metricRelease, family).Inc()
	return nil
}

// ReleaseIP release a IP address.
func (ipam *IPAM) ReleaseIP(ip net.IP) error {
	ipam.allocatorMutex.Lock()
	defer ipam.allocatorMutex.Unlock()
	return ipam.releaseIPLocked(ip)
}

// ReleaseIPString is identical to ReleaseIP but takes a string and supports
// referring to the IPs to be released with the IP itself or the owner name
// used during allocation. If the owner can be referred to multiple IPs, then
// all IPs are being released.
func (ipam *IPAM) ReleaseIPString(releaseArg string) (err error) {
	var ips []net.IP

	ip := net.ParseIP(releaseArg)
	if ip == nil {
		ips = ipam.lookupIPsByOwner(releaseArg)
		if len(ips) == 0 {
			return fmt.Errorf("Invalid IP address or owner name: %s", releaseArg)
		}
	} else {
		ips = append(ips, ip)
	}

	for _, parsedIP := range ips {
		// If any of the releases fail, report the failure
		if err2 := ipam.ReleaseIP(parsedIP); err2 != nil {
			err = err2
		}
	}
	return
}

// Dump dumps the list of allocated IP addresses
func (ipam *IPAM) Dump() (allocv4 map[string]string, allocv6 map[string]string, status string) {
	var st4, st6 string

	ipam.allocatorMutex.RLock()
	defer ipam.allocatorMutex.RUnlock()

	if ipam.IPv4Allocator != nil {
		allocv4, st4 = ipam.IPv4Allocator.Dump()
		st4 = "IPv4: " + st4
		for ip := range allocv4 {
			owner, _ := ipam.owner[ip]
			// If owner is not available, report IP but leave owner empty
			allocv4[ip] = owner
		}
	}

	if ipam.IPv6Allocator != nil {
		allocv6, st6 = ipam.IPv6Allocator.Dump()
		st6 = "IPv6: " + st6
		for ip := range allocv6 {
			owner, _ := ipam.owner[ip]
			// If owner is not available, report IP but leave owner empty
			allocv6[ip] = owner
		}
	}

	status = strings.Join([]string{st4, st6}, ", ")
	if status == "" {
		status = "Not running"
	}

	return
}

// StartExpirationTimer installs an expiration timer for a previously allocated
// IP. Unless StopExpirationTimer is called in time, the IP will be released
// again after expiration of the specified timeout. The function will return a
// UUID representing the unique allocation attempt. The same UUID must be
// passed into StopExpirationTimer again.
//
// This function is to be used as allocation and use of an IP can be controlled
// by an external entity and that external entity can disappear. Therefore such
// users should register an expiration timer before returning the IP and then
// stop the expiration timer when the IP has been used.
func (ipam *IPAM) StartExpirationTimer(ip net.IP, timeout time.Duration) (string, error) {
	ipam.allocatorMutex.Lock()
	defer ipam.allocatorMutex.Unlock()

	ipString := ip.String()
	if _, ok := ipam.expirationTimers[ipString]; ok {
		return "", fmt.Errorf("expiration timer already registered")
	}

	allocationUUID := uuid.NewUUID().String()
	ipam.expirationTimers[ipString] = allocationUUID

	go func(ip net.IP, allocationUUID string, timeout time.Duration) {
		ipString := ip.String()
		time.Sleep(timeout)

		ipam.allocatorMutex.Lock()
		defer ipam.allocatorMutex.Unlock()

		if currentUUID, ok := ipam.expirationTimers[ipString]; ok {
			if currentUUID == allocationUUID {
				scopedLog := log.WithFields(logrus.Fields{"ip": ipString, "uuid": allocationUUID})
				if err := ipam.releaseIPLocked(ip); err != nil {
					scopedLog.WithError(err).Warning("Unable to release IP after expiration")
				} else {
					scopedLog.Warning("Released IP after expiration")
				}
			} else {
				// This is an obsolete expiration timer. The IP
				// was reused and a new expiration timer is
				// already attached
			}
		} else {
			// Expiration timer was removed. No action is required
		}
	}(ip, allocationUUID, timeout)

	return allocationUUID, nil
}

// StopExpirationTimer will remove the expiration timer for a particular IP.
// The UUID returned by the symmetric StartExpirationTimer must be provided.
// The expiration timer will only be removed if the UUIDs match. Releasing an
// IP will also stop the expiration timer.
func (ipam *IPAM) StopExpirationTimer(ip net.IP, allocationUUID string) error {
	ipam.allocatorMutex.Lock()
	defer ipam.allocatorMutex.Unlock()

	ipString := ip.String()
	if currentUUID, ok := ipam.expirationTimers[ipString]; !ok {
		return fmt.Errorf("no expiration timer registered")
	} else if currentUUID != allocationUUID {
		return fmt.Errorf("UUID mismatch, not stopping expiration timer")
	}

	delete(ipam.expirationTimers, ipString)

	return nil
}
