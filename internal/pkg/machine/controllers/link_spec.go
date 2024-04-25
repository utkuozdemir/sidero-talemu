// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/hashicorp/go-multierror"
	"github.com/jsimonetti/rtnetlink"
	"github.com/siderolabs/gen/xslices"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	"go.uber.org/zap"
	"go4.org/netipx"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/siderolabs/talemu/internal/pkg/machine/network/watch"
)

// LinkSpecController applies network.LinkSpec to the actual interfaces.
type LinkSpecController struct{}

// Name implements controller.Controller interface.
func (ctrl *LinkSpecController) Name() string {
	return "network.LinkSpecController"
}

// Inputs implements controller.Controller interface.
func (ctrl *LinkSpecController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: network.NamespaceName,
			Type:      network.LinkSpecType,
			Kind:      controller.InputStrong,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *LinkSpecController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: network.LinkRefreshType,
			Kind: controller.OutputShared,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *LinkSpecController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	// watch link changes as some routes might need to be re-applied if the link appears
	watcher, err := watch.NewRtNetlink(watch.NewDefaultRateLimitedTrigger(ctx, r), unix.RTMGRP_LINK)
	if err != nil {
		return err
	}

	defer watcher.Done()

	conn, err := rtnetlink.Dial(nil)
	if err != nil {
		return fmt.Errorf("error dialing rtnetlink socket: %w", err)
	}

	defer conn.Close() //nolint:errcheck

	wgClient, err := wgctrl.New()
	if err != nil {
		logger.Warn("error creating wireguard client", zap.Error(err))
	} else {
		defer wgClient.Close() //nolint:errcheck
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		}

		// list source network configuration resources
		list, err := r.List(ctx, resource.NewMetadata(network.NamespaceName, network.LinkSpecType, "", resource.VersionUndefined))
		if err != nil {
			return fmt.Errorf("error listing source network addresses: %w", err)
		}

		// add finalizers for all live resources
		for _, res := range list.Items {
			if res.Metadata().Phase() != resource.PhaseRunning {
				continue
			}

			if err = r.AddFinalizer(ctx, res.Metadata(), ctrl.Name()); err != nil {
				return fmt.Errorf("error adding finalizer: %w", err)
			}
		}

		// list rtnetlink links (interfaces)
		links, err := conn.Link.List()
		if err != nil {
			return fmt.Errorf("error listing links: %w", err)
		}

		// loop over links and make reconcile decision
		var multiErr *multierror.Error

		for _, res := range list.Items {
			link := res.(*network.LinkSpec) //nolint:forcetypeassert,errcheck

			if err = ctrl.syncLink(ctx, r, logger, conn, wgClient, &links, link); err != nil {
				multiErr = multierror.Append(multiErr, err)
			}
		}

		if err = multiErr.ErrorOrNil(); err != nil {
			return err
		}

		r.ResetRestartBackoff()
	}
}

// FindLink looks up the link in the list of the links from rtnetlink.
func FindLink(links []rtnetlink.LinkMessage, name string) *rtnetlink.LinkMessage {
	index := slices.IndexFunc(links, func(link rtnetlink.LinkMessage) bool {
		return link.Attributes.Name == name
	})
	if index == -1 {
		return nil
	}

	return &links[index]
}

// syncLink syncs kernel state with the LinkSpec link.
//
// This method is really long, but it's hard to break it down in multiple pieces, are those pieces and steps are inter-dependent, so, instead,
// I'm going to provide high-level flow of the method here to help understand it:
//
// First of all, if the spec is being torn down - remove the link from the kernel, done.
// If the link spec is not being torn down, start the sync process:
//
//   - for physical links, there's not much we can sync - only MTU and 'UP' flag
//   - for logical links, controller handles creation and sync of the settings depending on the interface type
//
// If the logical link kind or type got changed (for example, "link0" was a bond, and now it's wireguard interface), the link
// is dropped and replaced with the new one.
// Same replace flow is used for VLAN links, as VLAN settings can't be changed on the fly.
//
// For bonded links, there are two sync steps applied:
//
//   - bond slave interfaces are enslaved to be part of the bond (by changing MasterIndex)
//   - bond master link settings are synced with the spec: some settings can't be applied on UP bond and a bond which has slaves,
//     so slaves are removed and bond is brought down (these settings are going to be reconciled back in the next sync cycle)
//
// For wireguard links, only settings are synced with the diff generated by the WireguardSpec.
//
//nolint:gocyclo,cyclop,gocognit,maintidx
func (ctrl *LinkSpecController) syncLink(ctx context.Context, r controller.Runtime, logger *zap.Logger, conn *rtnetlink.Conn, wgClient *wgctrl.Client,
	links *[]rtnetlink.LinkMessage, link *network.LinkSpec,
) error {
	logger = logger.With(zap.String("link", link.TypedSpec().Name))

	switch link.Metadata().Phase() {
	case resource.PhaseTearingDown:
		// TODO: should we bring link down if it's physical and the spec was torn down?
		if link.TypedSpec().Logical {
			existing := FindLink(*links, link.TypedSpec().Name)

			if existing != nil {
				if err := conn.Link.Delete(existing.Index); err != nil {
					return fmt.Errorf("error deleting link %q: %w", link.TypedSpec().Name, err)
				}

				logger.Info("deleted link")

				// refresh links as the link list got changed
				var err error

				*links, err = conn.Link.List()
				if err != nil {
					return fmt.Errorf("error listing links: %w", err)
				}
			}
		}

		// now remove finalizer as link was deleted
		if err := r.RemoveFinalizer(ctx, link.Metadata(), ctrl.Name()); err != nil {
			return fmt.Errorf("error removing finalizer: %w", err)
		}
	case resource.PhaseRunning:
		existing := FindLink(*links, link.TypedSpec().Name)

		// check if type/kind matches for the existing logical link
		if existing != nil && link.TypedSpec().Logical {
			replace := false

			if existing.Attributes.Info == nil {
				logger.Warn("requested logical link has no info, skipping sync",
					zap.String("name", existing.Attributes.Name),
					zap.Stringer("type", nethelpers.LinkType(existing.Type)),
					zap.Uint32("index", existing.Index),
				)

				return nil
			}

			// if type/kind doesn't match, recreate the link to change it
			if existing.Type != uint16(link.TypedSpec().Type) || existing.Attributes.Info.Kind != link.TypedSpec().Kind {
				logger.Info("replacing logical link",
					zap.String("old_kind", existing.Attributes.Info.Kind),
					zap.String("new_kind", link.TypedSpec().Kind),
					zap.Stringer("old_type", nethelpers.LinkType(existing.Type)),
					zap.Stringer("new_type", link.TypedSpec().Type),
				)

				replace = true
			}

			if replace {
				if err := conn.Link.Delete(existing.Index); err != nil {
					return fmt.Errorf("error deleting link %q: %w", link.TypedSpec().Name, err)
				}

				// not refreshing links, as the link is set to be re-created

				existing = nil
			}
		}

		if existing == nil {
			if !link.TypedSpec().Logical {
				// physical interface doesn't exist yet, nothing to be done
				return nil
			}

			// create logical interface
			var (
				parentIndex uint32
				data        []byte
				err         error
			)

			// skip any kinds of network interfaces except wireguard
			if link.TypedSpec().Kind != network.LinkKindWireguard {
				return nil
			}

			if err = conn.Link.New(&rtnetlink.LinkMessage{
				Type: uint16(link.TypedSpec().Type),
				Attributes: &rtnetlink.LinkAttributes{
					Name: link.TypedSpec().Name,
					Type: parentIndex,
					Info: &rtnetlink.LinkInfo{
						Kind: link.TypedSpec().Kind,
						Data: data,
					},
				},
			}); err != nil {
				return fmt.Errorf("error creating logical link %q: %w", link.TypedSpec().Name, err)
			}

			logger.Info("created new link", zap.String("kind", link.TypedSpec().Kind))

			// refresh links as the link list got changed
			*links, err = conn.Link.List()
			if err != nil {
				return fmt.Errorf("error listing links: %w", err)
			}

			existing = FindLink(*links, link.TypedSpec().Name)
			if existing == nil {
				return fmt.Errorf("created link %q not found in the link list", link.TypedSpec().Name)
			}
		}

		// sync wireguard settings
		if link.TypedSpec().Kind == network.LinkKindWireguard {
			if wgClient == nil {
				return fmt.Errorf("wireguard client not available, cannot configure wireguard link %q", link.TypedSpec().Name)
			}

			wgDev, err := wgClient.Device(link.TypedSpec().Name)
			if err != nil {
				return fmt.Errorf("error getting wireguard settings for %q: %w", link.TypedSpec().Name, err)
			}

			var existingSpec network.WireguardSpec

			WireguardSpec(&existingSpec).Decode(wgDev, false)
			existingSpec.Sort()

			link.TypedSpec().Wireguard.Sort()

			// order here is important: we allow listenPort to be zero in the configuration
			if !existingSpec.Equal(&link.TypedSpec().Wireguard) {
				config, err := WireguardSpec(&link.TypedSpec().Wireguard).Encode(&existingSpec)
				if err != nil {
					return fmt.Errorf("error creating wireguard config patch for %q: %w", link.TypedSpec().Name, err)
				}

				if err = wgClient.ConfigureDevice(link.TypedSpec().Name, *config); err != nil {
					return fmt.Errorf("error configuring wireguard device %q: %w", link.TypedSpec().Name, err)
				}

				logger.Info("reconfigured wireguard link", zap.Int("peers", len(link.TypedSpec().Wireguard.Peers)))

				// notify link status controller, as wireguard updates can't be watched via netlink API
				if err = safe.WriterModify[*network.LinkRefresh](ctx, r, network.NewLinkRefresh(network.NamespaceName, network.LinkKindWireguard), func(r *network.LinkRefresh) error {
					r.TypedSpec().Bump()

					return nil
				}); err != nil {
					return errors.New("error bumping link refresh")
				}
			}
		}

		// sync UP flag
		existingUp := existing.Flags&unix.IFF_UP == unix.IFF_UP
		if existingUp != link.TypedSpec().Up {
			flags := uint32(0)

			if link.TypedSpec().Up {
				flags = unix.IFF_UP
			}

			if err := conn.Link.Set(&rtnetlink.LinkMessage{
				Family: existing.Family,
				Type:   existing.Type,
				Index:  existing.Index,
				Flags:  flags,
				Change: unix.IFF_UP,
			}); err != nil {
				return fmt.Errorf("error changing flags for %q: %w", link.TypedSpec().Name, err)
			}

			logger.Debug("brought link up/down", zap.Bool("up", link.TypedSpec().Up))
		}

		// sync MTU if it's set in the spec
		if link.TypedSpec().MTU != 0 && existing.Attributes.MTU != link.TypedSpec().MTU {
			if err := conn.Link.Set(&rtnetlink.LinkMessage{
				Family: existing.Family,
				Type:   existing.Type,
				Index:  existing.Index,
				Attributes: &rtnetlink.LinkAttributes{
					MTU: link.TypedSpec().MTU,
				},
			}); err != nil {
				return fmt.Errorf("error setting MTU for %q: %w", link.TypedSpec().Name, err)
			}

			existing.Attributes.MTU = link.TypedSpec().MTU

			logger.Info("changed MTU for the link", zap.Uint32("mtu", link.TypedSpec().MTU))
		}
	}

	return nil
}

// WireguardSpec adapter provides encoding/decoding to netlink structures.
//
//nolint:revive,golint
func WireguardSpec(r *network.WireguardSpec) wireguardSpec {
	return wireguardSpec{
		WireguardSpec: r,
	}
}

type wireguardSpec struct {
	*network.WireguardSpec
}

// Encode converts WireguardSpec to wgctrl.Config "patch" to adjust the config to match the spec.
//
// Both specs should be sorted.
//
// Encode produces a "diff" as *wgtypes.Config which when applied transitions `existing` configuration into
// configuration `spec`.
//
//nolint:gocyclo,cyclop,gocognit
func (a wireguardSpec) Encode(existing *network.WireguardSpec) (*wgtypes.Config, error) {
	spec := a.WireguardSpec

	cfg := &wgtypes.Config{}

	if existing.PrivateKey != spec.PrivateKey {
		key, err := wgtypes.ParseKey(spec.PrivateKey)
		if err != nil {
			return nil, err
		}

		cfg.PrivateKey = &key
	}

	if existing.ListenPort != spec.ListenPort {
		cfg.ListenPort = &spec.ListenPort
	}

	if existing.FirewallMark != spec.FirewallMark {
		cfg.FirewallMark = &spec.FirewallMark
	}

	// perform a merge of two sorted list of peers producing diff
	l, r := 0, 0

	for l < len(existing.Peers) || r < len(spec.Peers) {
		addPeer := func(peer *network.WireguardPeer) error {
			pubKey, err := wgtypes.ParseKey(peer.PublicKey)
			if err != nil {
				return err
			}

			var presharedKey *wgtypes.Key

			if peer.PresharedKey != "" {
				var parsedKey wgtypes.Key

				parsedKey, err = wgtypes.ParseKey(peer.PresharedKey)
				if err != nil {
					return err
				}

				presharedKey = &parsedKey
			}

			var endpoint *net.UDPAddr

			if peer.Endpoint != "" {
				endpoint, err = net.ResolveUDPAddr("", peer.Endpoint)
				if err != nil {
					return err
				}
			}

			cfg.Peers = append(cfg.Peers, wgtypes.PeerConfig{
				PublicKey:                   pubKey,
				Endpoint:                    endpoint,
				PresharedKey:                presharedKey,
				PersistentKeepaliveInterval: &peer.PersistentKeepaliveInterval,
				ReplaceAllowedIPs:           true,
				AllowedIPs: xslices.Map(peer.AllowedIPs, func(peerIP netip.Prefix) net.IPNet {
					return *netipx.PrefixIPNet(peerIP)
				}),
			})

			return nil
		}

		deletePeer := func(peer *network.WireguardPeer) error {
			pubKey, err := wgtypes.ParseKey(peer.PublicKey)
			if err != nil {
				return err
			}

			cfg.Peers = append(cfg.Peers, wgtypes.PeerConfig{
				PublicKey: pubKey,
				Remove:    true,
			})

			return nil
		}

		var left, right *network.WireguardPeer

		if l < len(existing.Peers) {
			left = &existing.Peers[l]
		}

		if r < len(spec.Peers) {
			right = &spec.Peers[r]
		}

		switch {
		// peer from the "right" (new spec) is missing in "existing" (left), add it
		case left == nil || (right != nil && left.PublicKey > right.PublicKey):
			if err := addPeer(right); err != nil {
				return nil, err
			}

			r++
		// peer from the "left" (existing) is missing in new spec (right), so it should be removed
		case right == nil || (left != nil && left.PublicKey < right.PublicKey):
			// deleting peers from the existing
			if err := deletePeer(left); err != nil {
				return nil, err
			}

			l++
		// peer public keys are equal, so either they are identical or peer should be replaced
		case left.PublicKey == right.PublicKey:
			if !left.Equal(right) {
				// replace peer
				if err := addPeer(right); err != nil {
					return nil, err
				}
			}

			l++
			r++
		}
	}

	return cfg, nil
}

// Decode spec from the device state.
func (a wireguardSpec) Decode(dev *wgtypes.Device, isStatus bool) {
	spec := a.WireguardSpec

	if isStatus {
		spec.PublicKey = dev.PublicKey.String()
	} else {
		spec.PrivateKey = dev.PrivateKey.String()
	}

	spec.ListenPort = dev.ListenPort
	spec.FirewallMark = dev.FirewallMark

	spec.Peers = make([]network.WireguardPeer, len(dev.Peers))

	for i := range spec.Peers {
		spec.Peers[i].PublicKey = dev.Peers[i].PublicKey.String()

		if dev.Peers[i].Endpoint != nil {
			spec.Peers[i].Endpoint = dev.Peers[i].Endpoint.String()
		}

		var zeroKey wgtypes.Key

		if dev.Peers[i].PresharedKey != zeroKey {
			spec.Peers[i].PresharedKey = dev.Peers[i].PresharedKey.String()
		}

		spec.Peers[i].PersistentKeepaliveInterval = dev.Peers[i].PersistentKeepaliveInterval
		spec.Peers[i].AllowedIPs = xslices.Map(dev.Peers[i].AllowedIPs, func(peerIP net.IPNet) netip.Prefix {
			res, _ := netipx.FromStdIPNet(&peerIP)

			return res
		})
	}
}
