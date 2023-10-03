package main

import (
	"flag"
	"os"
	"strconv"

	"github.com/networkop/meshnet-cni/daemon/cni"
	"github.com/networkop/meshnet-cni/daemon/grpcwire"
	"github.com/networkop/meshnet-cni/daemon/meshnet"
	mpb "github.com/networkop/meshnet-cni/daemon/proto/meshnet/v1beta1"
	"github.com/networkop/meshnet-cni/daemon/vxlan"
	"github.com/networkop/meshnet-cni/utils/wireutil"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func main() {

	if err := cni.Init(); err != nil {
		log.Errorf("Failed to initialise CNI plugin: %v", err)
		os.Exit(1)
	}
	defer cni.Cleanup()

	isDebug := flag.Bool("d", false, "enable degugging")
	grpcPort, err := strconv.Atoi(os.Getenv("GRPC_PORT"))
	if err != nil || grpcPort == 0 {
		grpcPort = wireutil.GRPCDefaultPort
	}
	flag.Parse()
	log.SetLevel(log.InfoLevel)
	if *isDebug {
		log.SetLevel(log.DebugLevel)
		log.Debug("Verbose logging enabled")
	}
	// log.SetLevel(log.DebugLevel)

	meshnet.InitLogger()
	grpcwire.InitLogger()
	vxlan.InitLogger()

	m, err := meshnet.New(meshnet.Config{
		Port: grpcPort,
	})
	if err != nil {
		log.Errorf("failed to create meshnet: %v", err)
		os.Exit(1)
	}
	log.Info("Starting meshnet daemon...with grpc support")

	grpcwire.SetGWireClient(m.GWireDynClient)

	// read grpcwire info (if any) from data store and update local db
	err = grpcwire.ReconGWires()
	if err != nil {
		log.Errorf("could not reconcile grpc wire: %v", err)
		// generate error and continue
	}

	// Subscribe for links change event
	chLink := make(chan netlink.LinkUpdate)
	doneLink := make(chan struct{})
	defer close(doneLink)

	//if err := netlink.LinkSubscribeAt(netns.NsHandle(vethNs.Fd()), chLink, doneLink); err != nil {
	if err := netlink.LinkSubscribe(chLink, doneLink); err == nil {
		log.Infof("Subscribed to link change event")
	} else {
		log.Errorf("Could not subscribe to link change event: %v", err)
	}
	go expectLinkUpdate(chLink)

	if err := m.Serve(); err != nil {
		log.Errorf("daemon exited badly: %v", err)
		os.Exit(1)
	}
}

func expectLinkUpdate(ch <-chan netlink.LinkUpdate) bool {
	for {
		select {
		case update := <-ch:
			if update.Link != nil {
				log.Infof("expectLinkUpdate: Link name %s, oper state %d (%s), MTU %d, IFF_UP %d, flags %x(%d)",
					update.Link.Attrs().Name, update.Link.Attrs().OperState, update.Link.Attrs().OperState.String(),
					update.Link.Attrs().MTU, update.IfInfomsg.Flags&unix.IFF_UP,
					update.IfInfomsg.Flags, update.IfInfomsg.Flags)

				var linkState int32 = 0
				if update.Link.Attrs().OperState == netlink.OperUp {
					linkState = int32(mpb.LinkState_UP)
				} else if update.Link.Attrs().OperState == netlink.OperLowerLayerDown &&
					update.IfInfomsg.Flags&unix.IFF_LOWER_UP == 0 {
					/* || update.Link.Attrs().OperState == netlink.OperDown */
					linkState = int32(mpb.LinkState_DOWN)
				} else {
					log.Infof("expectLinkUpdate: Unsupported link change type %d(%s), flags %x on interface %s",
						update.Link.Attrs().OperState, update.Link.Attrs().OperState.String(),
						update.IfInfomsg.Flags, update.Link.Attrs().Name)
					continue
				}
				// +++TBD: should we restrict max thread count to say 20??
				go grpcwire.HandleGRPCLinkStateChange(update.Link, linkState)
			}
		}
	}
}
