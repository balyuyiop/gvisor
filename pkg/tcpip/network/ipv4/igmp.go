// Copyright 2020 The gVisor Authors.
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

package ipv4

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

const (
	// igmpV1PresentDefault is the initial state for igmpV1Present in the
	// igmpState. As per RFC 2236 Page 9 says "No IGMPv1 Router Present ... is
	// the initial state."
	igmpV1PresentDefault = 0

	// v1RouterPresentTimeout from RFC 2236 Section 8.11, Page 18
	// See note on igmpState.igmpV1Present for more detail.
	v1RouterPresentTimeout = 400 * time.Second

	// v1MaxRespTime from RFC 2236 Section 4, Page 5. "The IGMPv1 router
	// will send General Queries with the Max Response Time set to 0. This MUST
	// be interpreted as a value of 100 (10 seconds)."
	//
	// Note that the Max Response Time field is a value in units of deciseconds.
	v1MaxRespTime = 10 * time.Second

	// UnsolicitedReportIntervalMax is the maximum delay between sending
	// unsolicited IGMP reports.
	//
	// Obtained from RFC 2236 Section 8.10, Page 19.
	UnsolicitedReportIntervalMax = 10 * time.Second
)

// IGMPOptions holds options for IGMP.
type IGMPOptions struct {
	// Enabled indicates whether IGMP will be performed.
	//
	// When enabled, IGMP may transmit IGMP report and leave messages when
	// joining and leaving multicast groups respectively, and handle incoming
	// IGMP packets.
	Enabled bool
}

var _ ip.MulticastGroupProtocol = (*igmpState)(nil)

// igmpState is the per-interface IGMP state.
//
// igmpState.init() MUST be called after creating an IGMP state.
type igmpState struct {
	// The IPv4 endpoint this igmpState is for.
	ep   *endpoint
	opts IGMPOptions

	// igmpV1Present is for maintaining compatibility with IGMPv1 Routers, from
	// RFC 2236 Section 4 Page 6: "The IGMPv1 router expects Version 1
	// Membership Reports in response to its Queries, and will not pay
	// attention to Version 2 Membership Reports.  Therefore, a state variable
	// MUST be kept for each interface, describing whether the multicast
	// Querier on that interface is running IGMPv1 or IGMPv2.  This variable
	// MUST be based upon whether or not an IGMPv1 query was heard in the last
	// [Version 1 Router Present Timeout] seconds".
	//
	// Must be accessed with atomic operations. Holds a value of 1 when true, 0
	// when false.
	igmpV1Present uint32

	mu struct {
		sync.RWMutex

		genericMulticastProtocol ip.GenericMulticastProtocolState

		// igmpV1Job is scheduled when this interface receives an IGMPv1 style
		// message, upon expiration the igmpV1Present flag is cleared.
		// igmpV1Job may not be nil once igmpState is initialized.
		igmpV1Job *tcpip.Job
	}
}

// SendReport implements ip.MulticastGroupProtocol.
func (igmp *igmpState) SendReport(groupAddress tcpip.Address) *tcpip.Error {
	igmpType := header.IGMPv2MembershipReport
	if igmp.v1Present() {
		igmpType = header.IGMPv1MembershipReport
	}
	return igmp.writePacket(groupAddress, groupAddress, igmpType)
}

// SendLeave implements ip.MulticastGroupProtocol.
func (igmp *igmpState) SendLeave(groupAddress tcpip.Address) *tcpip.Error {
	// As per RFC 2236 Section 6, Page 8: "If the interface state says the
	// Querier is running IGMPv1, this action SHOULD be skipped. If the flag
	// saying we were the last host to report is cleared, this action MAY be
	// skipped."
	if igmp.v1Present() {
		return nil
	}
	return igmp.writePacket(header.IPv4AllRoutersGroup, groupAddress, header.IGMPLeaveGroup)
}

// init sets up an igmpState struct, and is required to be called before using
// a new igmpState.
func (igmp *igmpState) init(ep *endpoint, opts IGMPOptions) {
	igmp.mu.Lock()
	defer igmp.mu.Unlock()
	igmp.ep = ep
	igmp.opts = opts
	igmp.mu.genericMulticastProtocol.Init(ip.GenericMulticastProtocolOptions{
		Enabled:                   opts.Enabled,
		Rand:                      ep.protocol.stack.Rand(),
		Clock:                     ep.protocol.stack.Clock(),
		Protocol:                  igmp,
		MaxUnsolicitedReportDelay: UnsolicitedReportIntervalMax,
		AllNodesAddress:           header.IPv4AllSystems,
	})
	igmp.igmpV1Present = igmpV1PresentDefault
	igmp.mu.igmpV1Job = igmp.ep.protocol.stack.NewJob(&igmp.mu, func() {
		igmp.setV1Present(false)
	})
}

func (igmp *igmpState) handleIGMP(pkt *stack.PacketBuffer) {
	stats := igmp.ep.protocol.stack.Stats()
	received := stats.IGMP.PacketsReceived
	headerView, ok := pkt.Data.PullUp(header.IGMPMinimumSize)
	if !ok {
		received.Invalid.Increment()
		return
	}
	h := header.IGMP(headerView)

	// Temporarily reset the checksum field to 0 in order to calculate the proper
	// checksum.
	wantChecksum := h.Checksum()
	h.SetChecksum(0)
	gotChecksum := ^header.ChecksumVV(pkt.Data, 0 /* initial */)
	h.SetChecksum(wantChecksum)

	if gotChecksum != wantChecksum {
		received.ChecksumErrors.Increment()
		return
	}

	switch h.Type() {
	case header.IGMPMembershipQuery:
		received.MembershipQuery.Increment()
		if len(headerView) < header.IGMPQueryMinimumSize {
			received.Invalid.Increment()
			return
		}
		igmp.handleMembershipQuery(h.GroupAddress(), h.MaxRespTime())
	case header.IGMPv1MembershipReport:
		received.V1MembershipReport.Increment()
		if len(headerView) < header.IGMPReportMinimumSize {
			received.Invalid.Increment()
			return
		}
		igmp.handleMembershipReport(h.GroupAddress())
	case header.IGMPv2MembershipReport:
		received.V2MembershipReport.Increment()
		if len(headerView) < header.IGMPReportMinimumSize {
			received.Invalid.Increment()
			return
		}
		igmp.handleMembershipReport(h.GroupAddress())
	case header.IGMPLeaveGroup:
		received.LeaveGroup.Increment()
		// As per RFC 2236 Section 6, Page 7: "IGMP messages other than Query or
		// Report, are ignored in all states"

	default:
		// As per RFC 2236 Section 2.1 Page 3: "Unrecognized message types should
		// be silently ignored. New message types may be used by newer versions of
		// IGMP, by multicast routing protocols, or other uses."
		received.Unrecognized.Increment()
	}
}

func (igmp *igmpState) v1Present() bool {
	return atomic.LoadUint32(&igmp.igmpV1Present) == 1
}

func (igmp *igmpState) setV1Present(v bool) {
	if v {
		atomic.StoreUint32(&igmp.igmpV1Present, 1)
	} else {
		atomic.StoreUint32(&igmp.igmpV1Present, 0)
	}
}

func (igmp *igmpState) handleMembershipQuery(groupAddress tcpip.Address, maxRespTime time.Duration) {
	igmp.mu.Lock()
	defer igmp.mu.Unlock()

	// As per RFC 2236 Section 6, Page 10: If the maximum response time is zero
	// then change the state to note that an IGMPv1 router is present and
	// schedule the query received Job.
	if maxRespTime == 0 && igmp.opts.Enabled {
		igmp.mu.igmpV1Job.Cancel()
		igmp.mu.igmpV1Job.Schedule(v1RouterPresentTimeout)
		igmp.setV1Present(true)
		maxRespTime = v1MaxRespTime
	}

	igmp.mu.genericMulticastProtocol.HandleQuery(groupAddress, maxRespTime)
}

func (igmp *igmpState) handleMembershipReport(groupAddress tcpip.Address) {
	igmp.mu.Lock()
	defer igmp.mu.Unlock()
	igmp.mu.genericMulticastProtocol.HandleReport(groupAddress)
}

// writePacket assembles and sends an IGMP packet with the provided fields,
// incrementing the provided stat counter on success.
func (igmp *igmpState) writePacket(destAddress tcpip.Address, groupAddress tcpip.Address, igmpType header.IGMPType) *tcpip.Error {
	igmpData := header.IGMP(buffer.NewView(header.IGMPReportMinimumSize))
	igmpData.SetType(igmpType)
	igmpData.SetGroupAddress(groupAddress)
	igmpData.SetChecksum(header.IGMPCalculateChecksum(igmpData))

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		ReserveHeaderBytes: int(igmp.ep.MaxHeaderLength()),
		Data:               buffer.View(igmpData).ToVectorisedView(),
	})

	// TODO(gvisor.dev/issue/4888): We should not use the unspecified address,
	// rather we should select an appropriate local address.
	localAddr := header.IPv4Any
	igmp.ep.addIPHeader(localAddr, destAddress, pkt, stack.NetworkHeaderParams{
		Protocol: header.IGMPProtocolNumber,
		TTL:      header.IGMPTTL,
		TOS:      stack.DefaultTOS,
	})

	// TODO(b/162198658): set the ROUTER_ALERT option when sending Host
	// Membership Reports.
	sent := igmp.ep.protocol.stack.Stats().IGMP.PacketsSent
	if err := igmp.ep.nic.WritePacketToRemote(header.EthernetAddressFromMulticastIPv4Address(destAddress), nil /* gso */, ProtocolNumber, pkt); err != nil {
		sent.Dropped.Increment()
		return err
	}
	switch igmpType {
	case header.IGMPv1MembershipReport:
		sent.V1MembershipReport.Increment()
	case header.IGMPv2MembershipReport:
		sent.V2MembershipReport.Increment()
	case header.IGMPLeaveGroup:
		sent.LeaveGroup.Increment()
	default:
		panic(fmt.Sprintf("unrecognized igmp type = %d", igmpType))
	}
	return nil
}

// joinGroup handles adding a new group to the membership map, setting up the
// IGMP state for the group, and sending and scheduling the required
// messages.
//
// If the group already exists in the membership map, returns
// tcpip.ErrDuplicateAddress.
func (igmp *igmpState) joinGroup(groupAddress tcpip.Address) {
	igmp.mu.Lock()
	defer igmp.mu.Unlock()
	igmp.mu.genericMulticastProtocol.JoinGroup(groupAddress, !igmp.ep.Enabled() /* dontInitialize */)
}

// isInGroup returns true if the specified group has been joined locally.
func (igmp *igmpState) isInGroup(groupAddress tcpip.Address) bool {
	igmp.mu.Lock()
	defer igmp.mu.Unlock()
	return igmp.mu.genericMulticastProtocol.IsLocallyJoined(groupAddress)
}

// leaveGroup handles removing the group from the membership map, cancels any
// delay timers associated with that group, and sends the Leave Group message
// if required.
func (igmp *igmpState) leaveGroup(groupAddress tcpip.Address) *tcpip.Error {
	igmp.mu.Lock()
	defer igmp.mu.Unlock()

	// LeaveGroup returns false only if the group was not joined.
	if igmp.mu.genericMulticastProtocol.LeaveGroup(groupAddress) {
		return nil
	}

	return tcpip.ErrBadLocalAddress
}

// softLeaveAll leaves all groups from the perspective of IGMP, but remains
// joined locally.
func (igmp *igmpState) softLeaveAll() {
	igmp.mu.Lock()
	defer igmp.mu.Unlock()
	igmp.mu.genericMulticastProtocol.MakeAllNonMember()
}

// initializeAll attemps to initialize the IGMP state for each group that has
// been joined locally.
func (igmp *igmpState) initializeAll() {
	igmp.mu.Lock()
	defer igmp.mu.Unlock()
	igmp.mu.genericMulticastProtocol.InitializeGroups()
}
