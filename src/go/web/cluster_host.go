package web

import (
	"errors"
	"fmt"
	"net/http"

	"phenix/util/common"
	"phenix/util/mm"
)

// errInvalidClusterHost is returned for a host name that names no cluster
// host, or more than one.
var errInvalidClusterHost = errors.New("invalid cluster host")

// clusterHostName returns the name minimega gives the cluster host that host
// names. GET /hosts lists hosts by display name, without the suffixes that
// container deployments add, such as -minimega, and the UI schedules VMs on
// the names it lists; minimega schedules a VM only on a host by its own name.
// A name that matches no host is refused with errInvalidClusterHost: the
// minimega script writes the schedule as given, so a stored name holding a
// line break would run any minimega command when the experiment starts.
func clusterHostName(host string) (string, error) {
	return clusterHostResolver()(host)
}

// clusterHostResolver returns a function that does what clusterHostName does,
// listing the cluster hosts from minimega once, on its first call with a host.
func clusterHostResolver() func(string) (string, error) {
	var (
		hosts  mm.Hosts
		listed bool
	)

	return func(host string) (string, error) {
		if host == "" {
			return "", nil
		}

		if !listed {
			var err error

			hosts, err = mm.GetClusterHosts(false)
			if err != nil {
				return "", fmt.Errorf("looking up cluster host %s: %w", host, err)
			}

			listed = true
		}

		return matchClusterHost(hosts, host)
	}
}

func matchClusterHost(hosts mm.Hosts, host string) (string, error) {
	var matches []string

	for _, h := range hosts {
		if h.Name == host {
			return host, nil
		}

		if common.TrimHostnameSuffixes(h.Name) == host {
			matches = append(matches, h.Name)
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w %q: it names no host of the cluster", errInvalidClusterHost, host)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%w %q: it names %v", errInvalidClusterHost, host, matches)
	}
}

// clusterHostStatus returns the HTTP status for an error of clusterHostName.
func clusterHostStatus(err error) int {
	if errors.Is(err, errInvalidClusterHost) {
		return http.StatusBadRequest
	}

	return http.StatusInternalServerError
}
