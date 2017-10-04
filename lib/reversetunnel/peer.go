/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

*/

package reversetunnel

import (
	"fmt"
	"net"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/teleport/lib/utils/proxy"

	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
)

func newClusterPeers(clusterName string) *clusterPeers {
	return &clusterPeers{
		clusterName: clusterName,
		peers:       make(map[string]*clusterPeer),
	}
}

// clusterPeers is a collection of cluster peers to a given cluster
type clusterPeers struct {
	clusterName string
	peers       map[string]*clusterPeer
}

func (p *clusterPeers) pickPeer() (*clusterPeer, error) {
	var currentPeer *clusterPeer
	for _, peer := range p.peers {
		if currentPeer == nil || peer.connInfo.GetLastHeartbeat().After(currentPeer.connInfo.GetLastHeartbeat()) {
			currentPeer = peer
		}
	}
	if currentPeer == nil {
		return nil, trace.NotFound("no active peers found for %v")
	}
	return currentPeer, nil
}

func (p *clusterPeers) updatePeer(conn services.TunnelConnection) bool {
	peer, ok := p.peers[conn.GetName()]
	if !ok {
		return false
	}
	peer.connInfo = conn
	return true
}

func (p *clusterPeers) addPeer(peer *clusterPeer) {
	p.peers[peer.connInfo.GetName()] = peer
}

func (p *clusterPeers) removePeer(connInfo services.TunnelConnection) {
	delete(p.peers, connInfo.GetName())
}

func (p *clusterPeers) CachingAccessPoint() (auth.AccessPoint, error) {
	peer, err := p.pickPeer()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return peer.CachingAccessPoint()
}

func (p *clusterPeers) GetClient() (auth.ClientI, error) {
	peer, err := p.pickPeer()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return peer.GetClient()
}

func (p *clusterPeers) String() string {
	return fmt.Sprintf("clusterPeer(%v)", p.clusterName)
}

func (p *clusterPeers) GetStatus() string {
	peer, err := p.pickPeer()
	if err != nil {
		return RemoteSiteStatusOffline
	}
	return peer.GetStatus()
}

func (p *clusterPeers) GetName() string {
	return p.clusterName
}

func (p *clusterPeers) GetLastConnected() time.Time {
	peer, err := p.pickPeer()
	if err != nil {
		return time.Time{}
	}
	return peer.GetLastConnected()
}

// Dial is used to connect a requesting client (say, tsh) to an SSH server
// located in a remote connected site, the connection goes through the
// reverse proxy tunnel.
func (p *clusterPeers) Dial(from, to net.Addr) (conn net.Conn, err error) {
	peer, err := p.pickPeer()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return peer.Dial(from, to)
}

// newClusterPeer returns new cluster peer
func newClusterPeer(srv *server, connInfo services.TunnelConnection) (*clusterPeer, error) {
	clusterPeer := &clusterPeer{
		srv:      srv,
		connInfo: connInfo,
		log: log.WithFields(log.Fields{
			teleport.Component: teleport.ComponentReverseTunnel,
			teleport.ComponentFields: map[string]string{
				"cluster": connInfo.GetClusterName(),
				"side":    "server",
			},
		}),
	}

	dialer, err := NewProxyDialer(ProxyDialerConfig{
		Config: srv.ClientConfig(),
		Dial:   clusterPeer.Dial,
	})
	clt, err := auth.NewClient("http://stub:0", dialer.Dial)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	clusterPeer.clt = clt

	accessPoint, err := srv.newAccessPoint(clt, []string{"reverse", connInfo.GetClusterName()})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	clusterPeer.accessPoint = accessPoint

	return clusterPeer, nil
}

// clusterPeer is a remote cluster that has established
// a tunnel to the peers
type clusterPeer struct {
	log         *log.Entry
	clt         auth.ClientI
	accessPoint auth.AccessPoint
	connInfo    services.TunnelConnection
	srv         *server
}

func (s *clusterPeer) CachingAccessPoint() (auth.AccessPoint, error) {
	return s.accessPoint, nil
}

func (s *clusterPeer) GetClient() (auth.ClientI, error) {
	return s.clt, nil
}

func (s *clusterPeer) String() string {
	return fmt.Sprintf("clusterPeer(%v)", s.connInfo)
}

func (s *clusterPeer) GetStatus() string {
	diff := time.Now().Sub(s.connInfo.GetLastHeartbeat())
	if diff > defaults.ReverseTunnelOfflineThreshold {
		return RemoteSiteStatusOffline
	}
	return RemoteSiteStatusOnline
}

func (s *clusterPeer) GetName() string {
	return s.connInfo.GetClusterName()
}

func (s *clusterPeer) GetLastConnected() time.Time {
	return s.connInfo.GetLastHeartbeat()
}

// Dial is used to connect a requesting client (say, tsh) to an SSH server
// located in a remote connected site, the connection goes through the
// reverse proxy tunnel.
func (s *clusterPeer) Dial(from, to net.Addr) (conn net.Conn, err error) {
	s.log.Infof("[TUNNEL] forward connection to %v through the peer %v", to, s.connInfo.GetProxyAddr())

	client, err := proxy.DialWithDeadline(to.Network(), s.connInfo.GetProxyAddr(), s.srv.ClientConfig())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer client.Close()

	se, err := client.NewSession()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer se.Close()

	writer, err := se.StdinPipe()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	reader, err := se.StdoutPipe()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Request opening TCP connection to the remote host
	if err := se.RequestSubsystem(fmt.Sprintf("proxy:%v@%v", to, s.connInfo.GetClusterName())); err != nil {
		return nil, trace.Wrap(err)
	}

	return utils.NewPipeNetConn(
		reader,
		writer,
		se,
		from,
		to,
	), nil
}
