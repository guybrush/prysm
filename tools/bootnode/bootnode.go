/**
 * Bootnode
 *
 * A node which implements the DiscoveryV5 protocol for peer
 * discovery. The purpose of this service is to provide a starting point for
 * newly connected services to find other peers outside of their network.
 *
 * Usage: Run bootnode --help for flag options.
 */
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/btcsuite/btcd/btcec"
	gethlog "github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	ds "github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	kaddht "github.com/libp2p/go-libp2p-kad-dht"
	dhtopts "github.com/libp2p/go-libp2p-kad-dht/opts"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prysmaticlabs/go-bitfield"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	"github.com/prysmaticlabs/prysm/shared/iputils"
	"github.com/prysmaticlabs/prysm/shared/logutil"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/runutil"
	"github.com/prysmaticlabs/prysm/shared/version"
	"github.com/sirupsen/logrus"
	_ "go.uber.org/automaxprocs"
)

var (
	debug         = flag.Bool("debug", false, "Enable debug logging")
	logFileName   = flag.String("log-file", "", "Specify log filename, relative or absolute")
	privateKey    = flag.String("private", "", "Private key to use for peer ID")
	discv5port    = flag.Int("discv5-port", 4000, "Port to listen for discv5 connections")
	kademliaPort  = flag.Int("kad-port", 4500, "Port to listen for connections to kad DHT")
	metricsPort   = flag.Int("metrics-port", 5000, "Port to listen for connections")
	externalIP    = flag.String("external-ip", "", "External IP for the bootnode")
	disableKad    = flag.Bool("disable-kad", false, "Disables the bootnode from running kademlia dht")
	log           = logrus.WithField("prefix", "bootnode")
	kadPeersCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "bootstrap_node_kaddht_peers",
		Help: "The current number of kaddht peers of the bootstrap node",
	})
	discv5PeersCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "bootstrap_node_discv5_peers",
		Help: "The current number of discv5 peers of the bootstrap node",
	})
)

const dhtProtocol = "/prysm/0.0.0/dht"
const defaultIP = "127.0.0.1"

type handler struct {
	listener *discover.UDPv5
}

func main() {
	flag.Parse()

	if *logFileName != "" {
		if err := logutil.ConfigurePersistentLogging(*logFileName); err != nil {
			log.WithError(err).Error("Failed to configuring logging to disk.")
		}
	}

	fmt.Printf("Starting bootnode. Version: %s\n", version.GetVersion())

	if *debug {
		logrus.SetLevel(logrus.DebugLevel)

		// Geth specific logging.
		glogger := gethlog.NewGlogHandler(gethlog.StreamHandler(os.Stderr, gethlog.TerminalFormat(false)))
		glogger.Verbosity(gethlog.LvlTrace)
		gethlog.Root().SetHandler(glogger)

		log.Debug("Debug logging enabled.")
	}
	privKey, interfacePrivKey := extractPrivateKey()
	cfg := discover.Config{
		PrivateKey: privKey,
	}
	ipAddr, err := iputils.ExternalIPv4()
	if err != nil {
		log.Fatal(err)
	}
	listener := createListener(ipAddr, *discv5port, cfg)

	node := listener.Self()
	log.Infof("Running bootnode: %s", node.String())

	var dhtValue *kaddht.IpfsDHT
	if !*disableKad {
		dhtValue = startKademliaDHT(interfacePrivKey)
	}

	handler := &handler{
		listener: listener,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/p2p", handler.httpHandler)

	if err := http.ListenAndServe(fmt.Sprintf(":%d", *metricsPort), mux); err != nil {
		log.Fatalf("Failed to start server %v", err)
	}

	// Update metrics once per slot.
	slotDuration := time.Duration(params.BeaconConfig().SecondsPerSlot)
	runutil.RunEvery(context.Background(), slotDuration*time.Second, func() {
		updateMetrics(listener, dhtValue)
	})

	select {}
}

func startKademliaDHT(privKey crypto.PrivKey) *kaddht.IpfsDHT {
	if *debug {
		logging.SetDebugLogging()
	}

	ipAddr := defaultIP
	if *externalIP != "" {
		ipAddr = *externalIP
	}

	listen, err := ma.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%d", ipAddr, *kademliaPort))
	if err != nil {
		log.Fatalf("Failed to construct new multiaddress. %v", err)
	}
	opts := []libp2p.Option{
		libp2p.ListenAddrs(listen),
	}
	opts = append(opts, libp2p.Identity(privKey))

	ctx := context.Background()
	host, err := libp2p.New(ctx, opts...)
	if err != nil {
		log.Fatalf("Failed to create new host. %v", err)
	}

	dopts := []dhtopts.Option{
		dhtopts.Datastore(dsync.MutexWrap(ds.NewMapDatastore())),
		dhtopts.Protocols(
			dhtProtocol,
		),
	}

	dht, err := kaddht.New(ctx, host, dopts...)
	if err != nil {
		log.Fatalf("Failed to create new dht: %v", err)
	}
	if err := dht.Bootstrap(context.Background()); err != nil {
		log.Fatalf("Failed to bootstrap DHT. %v", err)
	}

	fmt.Printf("Running Kademlia DHT bootnode: /ip4/%s/tcp/%d/p2p/%s\n", ipAddr, *kademliaPort, host.ID().Pretty())
	return dht
}

func createListener(ipAddr string, port int, cfg discover.Config) *discover.UDPv5 {
	ip := net.ParseIP(ipAddr)
	if ip.To4() == nil {
		log.Fatalf("IPV4 address not provided instead %s was provided", ipAddr)
	}
	udpAddr := &net.UDPAddr{
		IP:   ip,
		Port: port,
	}
	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		log.Fatal(err)
	}
	localNode, err := createLocalNode(cfg.PrivateKey, ip, port)
	if err != nil {
		log.Fatal(err)
	}

	network, err := discover.ListenV5(conn, localNode, cfg)
	if err != nil {
		log.Fatal(err)
	}
	return network
}

func (h *handler) httpHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	write := func(w io.Writer, b []byte) {
		if _, err := w.Write(b); err != nil {
			log.WithError(err).Error("Failed to write to http response")
		}
	}
	allNodes := h.listener.AllNodes()
	write(w, []byte("Nodes stored in the table:\n"))
	for i, n := range allNodes {
		write(w, []byte(fmt.Sprintf("Node %d\n", i)))
		write(w, []byte(n.String()+"\n"))
		write(w, []byte("Node ID: "+n.ID().String()+"\n"))
		write(w, []byte("IP: "+n.IP().String()+"\n"))
		write(w, []byte(fmt.Sprintf("UDP Port: %d", n.UDP())+"\n"))
		write(w, []byte(fmt.Sprintf("TCP Port: %d", n.UDP())+"\n\n"))
	}
}

func createLocalNode(privKey *ecdsa.PrivateKey, ipAddr net.IP, port int) (*enode.LocalNode, error) {
	db, err := enode.OpenDB("")
	if err != nil {
		return nil, errors.Wrap(err, "Could not open node's peer database")
	}
	external := net.ParseIP(*externalIP)
	if *externalIP == "" {
		external = ipAddr
	}
	digest, err := helpers.ComputeForkDigest(params.BeaconConfig().GenesisForkVersion, params.BeaconConfig().ZeroHash[:])
	if err != nil {
		return nil, errors.Wrap(err, "Could not compute fork digest")
	}

	forkID := &pb.ENRForkID{
		CurrentForkDigest: digest[:],
		NextForkVersion:   params.BeaconConfig().GenesisForkVersion,
		NextForkEpoch:     params.BeaconConfig().FarFutureEpoch,
	}
	forkEntry, err := forkID.MarshalSSZ()
	if err != nil {
		return nil, errors.Wrap(err, "Could not marshal fork id")
	}

	localNode := enode.NewLocalNode(db, privKey)
	localNode.Set(enr.WithEntry("eth2", forkEntry))
	localNode.Set(enr.WithEntry("attnets", bitfield.NewBitvector64()))
	localNode.SetFallbackIP(external)
	localNode.SetFallbackUDP(port)

	return localNode, nil
}

func extractPrivateKey() (*ecdsa.PrivateKey, crypto.PrivKey) {
	var privKey *ecdsa.PrivateKey
	var interfaceKey crypto.PrivKey
	if *privateKey != "" {
		dst, err := hex.DecodeString(*privateKey)
		if err != nil {
			panic(err)
		}
		unmarshalledKey, err := crypto.UnmarshalSecp256k1PrivateKey(dst)
		if err != nil {
			panic(err)
		}
		interfaceKey = unmarshalledKey
		privKey = (*ecdsa.PrivateKey)((*btcec.PrivateKey)(unmarshalledKey.(*crypto.Secp256k1PrivateKey)))

	} else {
		privInterfaceKey, _, err := crypto.GenerateSecp256k1Key(rand.Reader)
		if err != nil {
			panic(err)
		}
		interfaceKey = privInterfaceKey
		privKey = (*ecdsa.PrivateKey)((*btcec.PrivateKey)(privInterfaceKey.(*crypto.Secp256k1PrivateKey)))
		log.Warning("No private key was provided. Using default/random private key")
		b, err := privInterfaceKey.Raw()
		if err != nil {
			panic(err)
		}
		log.Debugf("Private key %x", b)
	}

	return privKey, interfaceKey
}

func updateMetrics(listener *discover.UDPv5, dht *kaddht.IpfsDHT) {
	if dht != nil {
		kadPeersCount.Set(float64(len(dht.Host().Peerstore().Peers())))
	}
	if listener != nil {
		discv5PeersCount.Set(float64(len(listener.AllNodes())))
	}
}
