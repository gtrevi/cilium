// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package auth

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/cilium/cilium/pkg/auth/certs"
	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/maps/authmap"
	"github.com/cilium/cilium/pkg/policy"
)

// signalAuthKey used in the signalmap. Must reflect struct auth_key in the datapath
type signalAuthKey authmap.AuthKey

// low-cardinality stringer for metrics
func (key signalAuthKey) String() string {
	return policy.AuthType(key.AuthType).String()
}

type authManager struct {
	logger       logrus.FieldLogger
	ipCache      ipCache
	authHandlers map[policy.AuthType]authHandler
	authmap      authMap

	mutex                    lock.Mutex
	pending                  map[authKey]struct{}
	handleAuthenticationFunc func(a *authManager, k authKey, reAuth bool)
}

// ipCache is the set of interactions the auth manager performs with the IPCache
type ipCache interface {
	GetNodeIP(uint16) string
	GetNodeID(nodeIP net.IP) (nodeID uint16, exists bool)
}

// authHandler is responsible to handle authentication for a specific auth type
type authHandler interface {
	authenticate(*authRequest) (*authResponse, error)
	authType() policy.AuthType
	subscribeToRotatedIdentities() <-chan certs.CertificateRotationEvent
}

type authRequest struct {
	localIdentity  identity.NumericIdentity
	remoteIdentity identity.NumericIdentity
	remoteNodeIP   string
}

type authResponse struct {
	expirationTime time.Time
}

func newAuthManager(logger logrus.FieldLogger, authHandlers []authHandler, authmap authMap, ipCache ipCache) (*authManager, error) {
	ahs := map[policy.AuthType]authHandler{}
	for _, ah := range authHandlers {
		if ah == nil {
			continue
		}
		if _, ok := ahs[ah.authType()]; ok {
			return nil, fmt.Errorf("multiple handlers for auth type: %s", ah.authType())
		}
		ahs[ah.authType()] = ah
	}

	return &authManager{
		logger:                   logger,
		authHandlers:             ahs,
		authmap:                  authmap,
		ipCache:                  ipCache,
		pending:                  make(map[authKey]struct{}),
		handleAuthenticationFunc: handleAuthentication,
	}, nil
}

// handleAuthRequest receives auth required signals and spawns a new go routine for each authentication request.
func (a *authManager) handleAuthRequest(_ context.Context, key signalAuthKey) error {
	k := authKey{
		localIdentity:  identity.NumericIdentity(key.LocalIdentity),
		remoteIdentity: identity.NumericIdentity(key.RemoteIdentity),
		remoteNodeID:   key.RemoteNodeID,
		authType:       policy.AuthType(key.AuthType),
	}

	a.logger.
		WithField("key", k).
		Debug("Handle authentication request")

	a.handleAuthenticationFunc(a, k, false)

	return nil
}

func (a *authManager) handleCertificateRotationEvent(_ context.Context, event certs.CertificateRotationEvent) error {
	a.logger.
		WithField("identity", event.Identity).
		Debug("Handle certificate rotation event")

	all, err := a.authmap.All()
	if err != nil {
		return fmt.Errorf("failed to get all auth map entries: %w", err)
	}

	for k := range all {
		if k.localIdentity == event.Identity || k.remoteIdentity == event.Identity {
			a.handleAuthenticationFunc(a, k, true)
		}
	}

	return nil
}

func handleAuthentication(a *authManager, k authKey, reAuth bool) {
	if a.markPendingAuth(k) {
		go func(key authKey) {
			defer a.clearPendingAuth(key)

			if !reAuth {
				// Check if the auth is actually required, as we might have
				// updated the authmap since the datapath issued the auth
				// required signal.
				if i, err := a.authmap.Get(key); err == nil && i.expiration.After(time.Now()) {
					a.logger.
						WithField("key", key).
						Debug("Already authenticated, skipping authentication")
					return
				}
			}

			if err := a.authenticate(key); err != nil {
				a.logger.
					WithError(err).
					WithField("key", key).
					Warning("Failed to authenticate request")
			}
		}(k)
	}
}

// markPendingAuth checks if there is a pending authentication for the given key.
// If an auth is already pending returns false, otherwise marks the key as pending
// and returns true.
func (a *authManager) markPendingAuth(key authKey) bool {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, exists := a.pending[key]; exists {
		// Auth for this key is already pending
		return false
	}
	a.pending[key] = struct{}{}
	return true
}

// clearPendingAuth marks the pending authentication as finished.
func (a *authManager) clearPendingAuth(key authKey) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	delete(a.pending, key)
}

func (a *authManager) authenticate(key authKey) error {
	a.logger.
		WithField("auth_type", key.authType).
		WithField("local_identity", key.localIdentity).
		WithField("remote_identity", key.remoteIdentity).
		Debug("Policy is requiring authentication")

	// Authenticate according to the requested auth type
	h, ok := a.authHandlers[key.authType]
	if !ok {
		return fmt.Errorf("unknown requested auth type: %s", key.authType)
	}

	nodeIP := a.ipCache.GetNodeIP(key.remoteNodeID)
	if nodeIP == "" {
		return fmt.Errorf("remote node IP not available for node ID %d", key.remoteNodeID)
	}

	authReq := &authRequest{
		localIdentity:  key.localIdentity,
		remoteIdentity: key.remoteIdentity,
		remoteNodeIP:   nodeIP,
	}

	authResp, err := h.authenticate(authReq)
	if err != nil {
		return fmt.Errorf("failed to authenticate with auth type %s: %w", key.authType, err)
	}

	if err = a.updateAuthMap(key, authResp.expirationTime); err != nil {
		return fmt.Errorf("failed to update BPF map in datapath: %w", err)
	}

	a.logger.
		WithField("auth_type", key.authType).
		WithField("local_identity", key.localIdentity).
		WithField("remote_identity", key.remoteIdentity).
		WithField("remote_node_ip", nodeIP).
		Debug("Successfully authenticated")

	return nil
}

func (a *authManager) updateAuthMap(key authKey, expirationTime time.Time) error {
	val := authInfo{
		expiration: expirationTime,
	}

	if err := a.authmap.Update(key, val); err != nil {
		return fmt.Errorf("failed to write auth information to BPF map: %w", err)
	}

	return nil
}
