package main

// This is a CLI that lets you join a global permissionless CRDT-based
// database using CRDTs and IPFS.

import (
	"bufio"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	ds "github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/query"
	badger "github.com/ipfs/go-ds-badger2"
	crdt "github.com/ipfs/go-ds-crdt"
	logging "github.com/ipfs/go-log/v2"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	crypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	ipfslite "github.com/hsanjuan/ipfs-lite"
	"github.com/mitchellh/go-homedir"

	multiaddr "github.com/multiformats/go-multiaddr"
)

var (
	logger            = logging.Logger("globaldb")
	bootstrapNode     bool
	bootstrapNodeAddr string
	listen            multiaddr.Multiaddr

	topicName = "globaldb-example"
	netTopic  = "globaldb-example-net"
	config    = "globaldb-example"
)

func main() {
	fmt.Println("Is this a bootstrap node? (y/n): ")
	var isBootstrap string
	fmt.Scanln(&isBootstrap)
	if isBootstrap == "y" {
		bootstrapNode = true
	} else {
		bootstrapNode = false
	}

	port := 4000 + rand.Intn(1000)

	listen, _ = multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/" + strconv.Itoa(port))

	// Bootstrappers are using 1024 keys. See:
	// https://github.com/ipfs/infra/issues/378
	crypto.MinRsaKeyBits = 1024

	logging.SetLogLevel("*", "error")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir, err := homedir.Dir()
	if err != nil {
		logger.Fatal(err)
	}
	uniqueID := fmt.Sprintf("instance-%d", time.Now().UnixNano())
	// make folder in dir if not exists
	err = os.MkdirAll(filepath.Join(dir, config), 0755)

	data := filepath.Join(dir, config, uniqueID)
	dsopts := badger.DefaultOptions
	dsopts.WithInMemory(true)
	store, err := badger.NewDatastore(data, &dsopts)
	if err != nil {
		logger.Fatal(err)
	}
	defer store.Close()

	keyPath := filepath.Join(data, "key")
	var priv crypto.PrivKey
	_, err = os.Stat(keyPath)
	if os.IsNotExist(err) {
		priv, _, err = crypto.GenerateKeyPair(crypto.Ed25519, 1)
		if err != nil {
			logger.Fatal(err)
		}
		data, err := crypto.MarshalPrivateKey(priv)
		if err != nil {
			logger.Fatal(err)
		}
		err = os.WriteFile(keyPath, data, 0400)
		if err != nil {
			logger.Fatal(err)
		}
	} else if err != nil {
		logger.Fatal(err)
	} else {
		key, err := os.ReadFile(keyPath)
		if err != nil {
			logger.Fatal(err)
		}
		priv, err = crypto.UnmarshalPrivateKey(key)
		if err != nil {
			logger.Fatal(err)
		}

	}
	pid, err := peer.IDFromPublicKey(priv.GetPublic())
	if err != nil {
		logger.Fatal(err)
	}

	h, dht, err := ipfslite.SetupLibp2p(
		ctx,
		priv,
		nil,
		[]multiaddr.Multiaddr{listen},
		nil,
		ipfslite.Libp2pOptionsExtra...,
	)

	if err != nil {
		logger.Fatal(err)
	}
	defer h.Close()
	defer dht.Close()

	psub, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		logger.Fatal(err)
	}

	topic, err := psub.Join(netTopic)
	if err != nil {
		logger.Fatal(err)
	}

	netSubs, err := topic.Subscribe()
	if err != nil {
		logger.Fatal(err)
	}

	// Use a special pubsub topic to avoid disconnecting
	// from globaldb peers.
	go func() {
		for {
			msg, err := netSubs.Next(ctx)
			if err != nil {
				fmt.Println(err)
				break
			}
			h.ConnManager().TagPeer(msg.ReceivedFrom, "keep", 100)
		}
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				topic.Publish(ctx, []byte("hi!"))
				time.Sleep(20 * time.Second)
			}
		}
	}()

	ipfs, err := ipfslite.New(ctx, store, nil, h, dht, nil)
	if err != nil {
		logger.Fatal(err)
	}

	psubCtx, psubCancel := context.WithCancel(ctx)
	pubsubBC, err := crdt.NewPubSubBroadcaster(psubCtx, psub, topicName)
	if err != nil {
		logger.Fatal(err)
	}

	opts := crdt.DefaultOptions()
	opts.Logger = logger
	opts.RebroadcastInterval = 5 * time.Second
	opts.PutHook = func(k ds.Key, v []byte) {
		fmt.Printf("Added: [%s] -> %s\n", k, string(v))

	}
	opts.DeleteHook = func(k ds.Key) {
		fmt.Printf("Removed: [%s]\n", k)
	}

	crdt, err := crdt.New(store, ds.NewKey("crdt"), ipfs, pubsubBC, opts)
	if err != nil {
		logger.Fatal(err)
	}
	defer crdt.Close()
	defer psubCancel()

	// if not bootstrapping, ask for bootstrap node address
	if !bootstrapNode {
		fmt.Println("Enter the bootstrap node address:")
		fmt.Scanln(&bootstrapNodeAddr)
		fmt.Println("Bootstrapping...")
		// pass bootstrap node address via command line

		bstr, _ := multiaddr.NewMultiaddr(bootstrapNodeAddr)

		inf, _ := peer.AddrInfoFromP2pAddr(bstr)
		list := append(ipfslite.DefaultBootstrapPeers(), *inf)
		ipfs.Bootstrap(list)
		h.ConnManager().TagPeer(inf.ID, "keep", 100)
	}

	myNodeAddr := listen.String() + "/ipfs/" + pid.String()

	fmt.Printf(`
Peer ID: %s
Listen address: %s
Topic: %s
Data Folder: %s
Node Address: %s
Ready!

Commands:

> list               -> list items in the store
> get <key>          -> get value for a key
> put <key> <value>  -> store value on a key
> exit               -> quit


`,
		pid, listen, topicName, data, myNodeAddr,
	)

	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		fmt.Println("Running in daemon mode")
		go func() {
			for {
				fmt.Printf("%s - %d connected peers\n", time.Now().Format(time.Stamp), len(connectedPeers(h)))
				time.Sleep(10 * time.Second)
			}
		}()
		signalChan := make(chan os.Signal, 20)
		signal.Notify(
			signalChan,
			syscall.SIGINT,
			syscall.SIGTERM,
			syscall.SIGHUP,
		)
		<-signalChan
		return
	}

	fmt.Printf("> ")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		text := scanner.Text()
		fields := strings.Fields(text)
		if len(fields) == 0 {
			fmt.Printf("> ")
			continue
		}

		cmd := fields[0]

		switch cmd {
		case "exit", "quit":
			return
		case "debug":
			if len(fields) < 2 {
				fmt.Println("debug <on/off/peers>")
			}
			st := fields[1]
			switch st {
			case "on":
				logging.SetLogLevel("globaldb", "debug")
			case "off":
				logging.SetLogLevel("globaldb", "error")
			case "peers":
				for _, p := range connectedPeers(h) {
					addrs, err := peer.AddrInfoToP2pAddrs(p)
					if err != nil {
						logger.Warn(err)
						continue
					}
					for _, a := range addrs {
						fmt.Println(a)
					}
				}
			}
		case "list":
			q := query.Query{}
			results, err := crdt.Query(ctx, q)
			if err != nil {
				printErr(err)
			}
			for r := range results.Next() {
				if r.Error != nil {
					printErr(err)
					continue
				}
				fmt.Printf("[%s] -> %s\n", r.Key, string(r.Value))
			}
		case "get":
			if len(fields) < 2 {
				fmt.Println("get <key>")
				fmt.Println("> ")
				continue
			}
			k := ds.NewKey(fields[1])
			v, err := crdt.Get(ctx, k)
			if err != nil {
				printErr(err)
				continue
			}
			fmt.Printf("[%s] -> %s\n", k, string(v))
		case "put":
			if len(fields) < 3 {
				fmt.Println("put <key> <value>")
				fmt.Println("> ")
				continue
			}
			k := ds.NewKey(fields[1])
			v := strings.Join(fields[2:], " ")
			err := crdt.Put(ctx, k, []byte(v))
			if err != nil {
				printErr(err)
				continue
			}
		}
		fmt.Printf("> ")
	}
}

func printErr(err error) {
	fmt.Println("error:", err)
	fmt.Println("> ")
}

func connectedPeers(h host.Host) []*peer.AddrInfo {
	var pinfos []*peer.AddrInfo
	for _, c := range h.Network().Conns() {
		pinfos = append(pinfos, &peer.AddrInfo{
			ID:    c.RemotePeer(),
			Addrs: []multiaddr.Multiaddr{c.RemoteMultiaddr()},
		})
	}
	return pinfos
}
