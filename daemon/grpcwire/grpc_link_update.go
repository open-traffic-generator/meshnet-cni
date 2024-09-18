package grpcwire

import (
	"context"
	"fmt"
	"strings"
	"time"

	grpcwirev1 "github.com/networkop/meshnet-cni/api/types/v1beta1"
	mpb "github.com/networkop/meshnet-cni/daemon/proto/meshnet/v1beta1"
	"github.com/networkop/meshnet-cni/utils/wireutil"
	"github.com/vishvananda/netlink"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
)

// Current Issues to solve:
// - how to not call callback during topo creation? i.e., how to avoid unnecessary callback handling?
// - how to make lookup faster for gwirestatus item?
// - LinkSetUp() on remote node is sending back linkstate up followed by linkstate down which is
//   causing local node to down its link state
//		- link down has been handled by flags. need to handle (avoid) link up event

// - should we restrict max thread count to say 20?? (done)
// - ifconfig up cmd is not making interface up. need to call it at both ends of veth link. (done)

const (
	kLinkState                    = "link_state" // json name of Status of gwire_type, +++TBD: can we make it dynamic
	kLinkStateUpdateRetryCount    = 4            // how many times to retry
	kLinkStateUpdateRetryInterval = 500          // msec

)

type linkStateEvent int

func (ls linkStateEvent) String() string {
	switch ls {
	case linkStateEvent(mpb.LinkState_DOWN):
		return "down"
	case linkStateEvent(mpb.LinkState_DOWN_UPDATING):
		return "down updating"
	case linkStateEvent(mpb.LinkState_UP):
		return "up"
	case linkStateEvent(mpb.LinkState_UP_UPDATING):
		return "up updating"
	}
	return "unknown"
}

// --------------------------------------------------------------------------------------------------------------
// HandleGRPCLinkStateChange
//   - find wirestatus from K8S data store for the given local node interface name and local node
//   - extract corresponding peer node ip and interface id on the peer node
//   - set given link state on the interface of peer node.
//   - update link state for this wirestatus in K8S datastore
func HandleGRPCLinkStateChange(link netlink.Link, ls int32) error {

	linkState := linkStateEvent(ls)
	intfName := link.Attrs().Name

	// grpcOvrlyLogger.Infof("HandleGRPCLinkStateChange: Update link state to %s on interface %s", linkState, intfName)
	// get wire status from K8S datastore
	wireStatus, err := getWireStatusFromK8SDatastore(intfName)
	if err != nil {
		grpcOvrlyLogger.Errorf("HandleGRPCLinkStateChange: Could not retrieve wireStatus, err %v", err)
		return err
	}

	// if this is repeated link state update then ignore it
	if (linkState == linkStateEvent(mpb.LinkState_DOWN)) && (wireStatus.LinkState == int64(mpb.LinkState_DOWN) || wireStatus.LinkState == int64(mpb.LinkState_DOWN_UPDATING)) {
		grpcOvrlyLogger.Infof("HandleGRPCLinkStateChange: Ignoring duplicate link state down notification on interface %s", intfName)
		return nil
	} else if (linkState == linkStateEvent(mpb.LinkState_UP)) && (wireStatus.LinkState == int64(mpb.LinkState_UP) || wireStatus.LinkState == int64(mpb.LinkState_UP_UPDATING)) {
		grpcOvrlyLogger.Infof("HandleGRPCLinkStateChange: Ignoring duplicate link state up notification on interface %s", intfName)
		return nil
	}

	if linkState == linkStateEvent(mpb.LinkState_UP) {
		if err := netlink.LinkSetUp(link); err != nil {
			grpcOvrlyLogger.Errorf("HandleGRPCLinkStateChange: Could not set link state up on interface %s, err %v",
				intfName, err)
		}
	}

	// grpcOvrlyLogger.Errorf("=================before peer node=============%d", wireStatus.WireIfaceIdOnPeerNode)
	// set link state on the interface of peer node
	err = updateGRPCLinkStateOnPeerNodeIntf(linkState, wireStatus.WireIfaceIdOnPeerNode, wireStatus.GWirePeerNodeIp)
	if err != nil {
		grpcOvrlyLogger.Errorf("HandleGRPCLinkStateChange: Could not update link state %s on peer node interface %d, err %v",
			linkState, wireStatus.WireIfaceIdOnPeerNode, err)
		return err
	}
	// grpcOvrlyLogger.Errorf("=================after peer node=============%d", wireStatus.WireIfaceIdOnPeerNode)
	// return nil

	// update link state in the K8S datastore
	err = updateWireStatusToK8SDatastore(linkState, wireStatus.WireIfaceNameOnLocalNode)
	if err != nil {
		grpcOvrlyLogger.Errorf("HandleGRPCLinkStateChange: Could not update link state %s for interface %s to K8S datastore, err %v",
			linkState, intfName, err)
		return err
	}
	// grpcOvrlyLogger.Infof("HandleGRPCLinkStateChange-done: Update link state to %s on interface %s", linkState, intfName)
	return nil
}

// -------------------------------------------------------------------------------------------------------------
// getWireStatusFromK8SDatastore gets GWireStatus for the given interface 'intfName' on the local node. 'intfName'
// is unique on this node.
func getWireStatusFromK8SDatastore(intfName string) (*grpcwirev1.GWireStatus, error) {
	var wireStatus grpcwirev1.GWireStatus
	nodeName, err := findNodeName()
	if err != nil {
		grpcOvrlyLogger.Errorf("getWireStatusFromK8SDatastore: Could not retrieve node, err: %v", err)
		return &wireStatus, err
	}

	ctx := context.Background()
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// retrieve list of grpc wire obj list for all namespaces for the current node-name
		gwireKObjList, err := gWClient.GetWireObjListUS(ctx, nodeName)
		if err != nil {
			grpcOvrlyLogger.Errorf("getWireStatusFromK8SDatastore: Could not get gWireKObjs list from K8S, err: %v", err)
			return err
		}
		// in the unlikely situation where one has multiple topologies running in the same cluster,
		// gwireKObjList will have multiple items for this node.
		// {(<node-1><topo-namespace-1>),(<node-1><topo-namespace-2>),...}
		for _, node := range gwireKObjList.Items {
			// a node is found and node-Status-GWireKItems exists, so reconcile
			grpcWireItems, found, err := unstructured.NestedSlice(node.Object, kStatus, kGrpcWireItems)
			if err != nil {
				grpcOvrlyLogger.Errorf("getWireStatusFromK8SDatastore: Could not retrieve grpcWireItem, err: %v", err)
				continue
			}
			if !found {
				grpcOvrlyLogger.Errorf("getWireStatusFromK8SDatastore: grpcWireItem not found in GWireKObj status, retrieved from k8s data-store")
				continue
			}
			if grpcWireItems == nil {
				grpcOvrlyLogger.Errorf("getWireStatusFromK8SDatastore: grpcWireItem is nil in GWireKObj status, retrieved from k8s data-store")
				continue
			}

			for _, grpcWireItem := range grpcWireItems {
				wireStatusItem, ok := grpcWireItem.(map[string]interface{})
				if !ok {
					grpcOvrlyLogger.Errorf("getWireStatusFromK8SDatastore: Unable to retrieve wire status item, grpcWireItem is not a map")
					continue
				}

				// create the wire structure from the saved data in K8S datastore
				wireStatus = grpcwirev1.GWireStatus{}
				if err := apiruntime.DefaultUnstructuredConverter.FromUnstructured(wireStatusItem, &wireStatus); err != nil {
					grpcOvrlyLogger.Errorf("getWireStatusFromK8SDatastore: Unable to retrieve wire status, err: %v", err)
					continue
				}
				if intfName == wireStatus.WireIfaceNameOnLocalNode { // this iface name is unique in a node
					return nil
				}
			}
		}
		return fmt.Errorf("getWireStatusFromK8SDatastore: Interface %s not present in K8S datastore for node %s", intfName, nodeName)
	})
	return &wireStatus, retryErr
}

// -------------------------------------------------------------------------------------------------------------
// updateWireStatusToK8SDatastore updates given link state 'linkState' for the given interface 'intfName' for the
// local node in K8S datastore.
func updateWireStatusToK8SDatastore(linkState linkStateEvent, intfName string) error {
	var wireStatus grpcwirev1.GWireStatus
	nodeName, err := findNodeName()
	if err != nil {
		grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: Could not get node: %v", err)
		return err
	}

	ctx := context.Background()
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// retrieve list of grpc wire obj list for all namespaces for the current node-name
		gwireKObjList, err := gWClient.GetWireObjListUS(ctx, nodeName)
		if err != nil {
			grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: Could not get gWireKObjs list from K8S, err: %v", err)
			return err
		}
		// in the unlikely situation where one has multiple topologies running in the same cluster,
		// gwireKObjList will have multiple items for this node.
		// {(<node-1><topo-namespace-1>),(<node-1><topo-namespace-2>),...}
		for _, node := range gwireKObjList.Items {
			// a node is found and node-Status-GWireKItems exists, so reconcile
			grpcWireItems, found, err := unstructured.NestedSlice(node.Object, kStatus, kGrpcWireItems)
			if err != nil {
				grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: Could not retrieve grpcWireItem, err: %v", err)
				continue
			}
			if !found {
				grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: GrpcWireItem not found in GWireKObj status, retrieved from k8s data-store")
				continue
			}
			if grpcWireItems == nil {
				grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: GrpcWireItem is nil in GWireKObj status, retrieved from k8s data-store")
				continue
			}

			var wireFound bool = false
			var grpcWireItem interface{}
			var wireStatusIndex int = 0
			for wireStatusIndex, grpcWireItem = range grpcWireItems {
				wireStatusItem, ok := grpcWireItem.(map[string]interface{})
				if !ok {
					grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: Unable to retrieve wire status item, grpcWireItem is not a map")
					continue
				}

				// create the wire structure from the saved data in K8S data store
				wireStatus = grpcwirev1.GWireStatus{}
				if err := apiruntime.DefaultUnstructuredConverter.FromUnstructured(wireStatusItem, &wireStatus); err != nil {
					grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: Unable to retrieve wire status, err: %v", err)
					continue
				}
				if intfName == wireStatus.WireIfaceNameOnLocalNode { // this iface name is unique in a node
					wireFound = true
					break
				}
			}

			if wireFound {
				// Update K8S data store

				// update linkState in wireStatusItem
				if err := unstructured.SetNestedField(grpcWireItems[wireStatusIndex].(map[string]interface{}), int64(linkState), kLinkState); err != nil {
					grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: Could not set link state %s for interface %s in wireStatusItem in K8S datastore, err %v",
						linkState, intfName, err)
					return err
				}
				if err := unstructured.SetNestedField(node.Object, grpcWireItems, kStatus, kGrpcWireItems); err != nil {
					grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: Could not set grpcWireItems in K8S datastore, err %v", err)
					return err
				}

				_, err = gWClient.UpdateWireObj(ctx, wireStatus.TopoNamespace, &node)
				if err != nil {
					return err
				}

				// outer loop break
				return nil
			}
		}
		return fmt.Errorf("updateWireStatusToK8SDatastore: Could not find interface %s in K8S datastore for node %s", intfName, nodeName)
	})
	return retryErr
}

// -------------------------------------------------------------------------------------------------------------
// updateGRPCLinkStateOnPeerNodeIntf sets given link state 'linkState' on the interface identified by 'peerIntfId'
// on peer node 'peerNodeIp'. It attempts 'linkStateUpdateRetryCount' times at an interval of
// 'linkStateUpdateRetryInterval' msec to set this link state.
func updateGRPCLinkStateOnPeerNodeIntf(linkState linkStateEvent, peerIntfId int64, peerNodeIp string) error {
	lsMsg := &mpb.LinkStateMessage{
		LinkState:  int32(linkState),
		PeerIntfId: peerIntfId,
	}

	// +++TBD: should we make this channel persistent??
	url := strings.TrimSpace(fmt.Sprintf("%s:%d", peerNodeIp, wireutil.GRPCDefaultPort))
	remote, err := grpc.Dial(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		grpcOvrlyLogger.Errorf("updateGRPCLinkStateOnPeerNodeIntf: Could not connect to remote %s, err: %v", url, err)
		return err
	}
	// grpcOvrlyLogger.Infof("updateGRPCLinkStateOnPeerNodeIntf: Successfully connected to remote %s", url)
	defer remote.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	remoteClient := mpb.NewRemoteClient(remote)

	resp, err := remoteClient.LinkStateUpdateRemote(ctx, lsMsg)
	if err != nil || !resp.Response {
		ticker := time.NewTicker(time.Millisecond * kLinkStateUpdateRetryInterval)
		defer ticker.Stop()

		iteration := 1
		for range ticker.C {
			resp, err := remoteClient.LinkStateUpdateRemote(context.Background(), lsMsg)
			if err != nil || !resp.Response {
				iteration++
				if iteration >= kLinkStateUpdateRetryCount {
					grpcOvrlyLogger.Errorf("updateGRPCLinkStateOnPeerNodeIntf: Could not update link state %s on peer node interface %d "+
						"after %d iterations", linkState, peerIntfId, iteration)
					return err
				}
			} else {
				break
			}
		}
	}

	// grpcOvrlyLogger.Infof("updateGRPCLinkStateOnPeerNodeIntf: Successfully updated link state %s on peer node interface %d",
	// 	linkState, peerIntfId)
	return nil
}

// ---------------------------------------------------------------------------------------------------------------------
// syncLinkStateWithPeer
//   - check if there is discrepancy of link state between given wire status stored in K8S datastore and
//     interface present local node
//   - if discrepancy is present then link state of interface (not of wire status) is set on the corresponding
//     interface on peer node
//   - link state is updated in K8S data store on successful update of link state on peer node
func syncLinkStateWithPeer(wireStatus grpcwirev1.GWireStatus) error {
	var (
		linkState           linkStateEvent = 0
		updateLinkStateInDs                = false
	)
	// check if sync is required for this local interface
	link, err := netlink.LinkByName(wireStatus.WireIfaceNameOnLocalNode)
	if err != nil {
		grpcOvrlyLogger.Errorf("syncLinkStateWithPeer: Interface %s does not exist on local node, err: %v",
			wireStatus.WireIfaceNameOnLocalNode, err)
		return err
	}
	if link.Attrs().OperState == netlink.OperUp && wireStatus.LinkState == int64(mpb.LinkState_DOWN) {
		// local interface is up and wirestatus is down, sync remote to up
		linkState = linkStateEvent(mpb.LinkState_UP)
		grpcOvrlyLogger.Infof("syncLinkStateWithPeer: Sync-ing remote wire interface (%d) to up because local"+
			" interface (%s) is up but wirestatus is down",
			wireStatus.WireIfaceIdOnPeerNode, wireStatus.WireIfaceNameOnLocalNode)
		err = updateGRPCLinkStateOnPeerNodeIntf(linkState, wireStatus.WireIfaceIdOnPeerNode, wireStatus.GWirePeerNodeIp)
		updateLinkStateInDs = true
	} else if (link.Attrs().OperState == netlink.OperLowerLayerDown || link.Attrs().OperState == netlink.OperDown) && wireStatus.LinkState == int64(mpb.LinkState_UP) {
		// local interface is down and wirestatus is up, sync remote to down
		linkState = linkStateEvent(mpb.LinkState_DOWN)
		grpcOvrlyLogger.Infof("syncLinkStateWithPeer: Sync-ing remote wire interface (%d) to down because local"+
			" interface (%s) is down but wirestatus is up",
			wireStatus.WireIfaceIdOnPeerNode, wireStatus.WireIfaceNameOnLocalNode)
		err = updateGRPCLinkStateOnPeerNodeIntf(linkState, wireStatus.WireIfaceIdOnPeerNode, wireStatus.GWirePeerNodeIp)
		updateLinkStateInDs = true
	}
	if err != nil {
		grpcOvrlyLogger.Errorf("syncLinkStateWithPeer: Could not update link state %s on peer node interface (%d), err: %v",
			linkState, wireStatus.WireIfaceIdOnPeerNode, err)
		return err
	}
	if updateLinkStateInDs {
		err = updateWireStatusToK8SDatastore(linkState, wireStatus.WireIfaceNameOnLocalNode)
		if err != nil {
			grpcOvrlyLogger.Errorf("syncLinkStateWithPeer: Could not update link state %s for interface %s to K8S datastore, err %v",
				linkState, wireStatus.WireIfaceNameOnLocalNode, err)
			return err
		}
	}

	return nil
}

// -------------------------------------------------------------------------------------------------------------
// TriggeredRemoteLinkStateUpdate sets given link state 'linkState' on the interface identified by id 'intfId'
// on local node.
func TriggeredRemoteLinkStateUpdate(ls int32, intfId int64) error {
	linkState := linkStateEvent(ls)
	// grpcOvrlyLogger.Errorf("TriggeredRemoteLinkStateUpdate: Updating link state %s on interface %d",
	// 	linkState, intfId)

	link, err := netlink.LinkByIndex(int(intfId))
	if err != nil {
		grpcOvrlyLogger.Errorf("TriggeredRemoteLinkStateUpdate: Could not retrieve interface %d, err %v", intfId, err)
		return err
	}

	wireStatus, err := getWireStatusFromK8SDatastore(link.Attrs().Name)
	if err != nil {
		grpcOvrlyLogger.Errorf("TriggeredRemoteLinkStateUpdate: Could not retrieve wireStatus, err %v", err)
		return err
	}

	// if this is repeated link state update then ignore it
	if linkState == linkStateEvent(mpb.LinkState_DOWN) && (wireStatus.LinkState == int64(mpb.LinkState_DOWN) || wireStatus.LinkState == int64(mpb.LinkState_DOWN_UPDATING)) {
		grpcOvrlyLogger.Infof("HandleGRPCLinkStateChange: Ignoring duplicate link state down instruction on interface %s", link.Attrs().Name)
		return nil
	} else if linkState == linkStateEvent(mpb.LinkState_UP) && (wireStatus.LinkState == int64(mpb.LinkState_UP) || wireStatus.LinkState == int64(mpb.LinkState_UP_UPDATING)) {
		grpcOvrlyLogger.Infof("HandleGRPCLinkStateChange: Ignoring duplicate link state up instruction on interface %s", link.Attrs().Name)
		return nil
	}

	// update link state to k8s datastore so that subsequent duplicate changes because of following
	// up/down instruction can be avoided. The intended state will be set after successful operation.
	localLinkState := linkStateEvent(mpb.LinkState_UP_UPDATING)
	if linkState == linkStateEvent(mpb.LinkState_DOWN) {
		localLinkState = linkStateEvent(mpb.LinkState_DOWN_UPDATING)
	}

	err = updateWireStatusToK8SDatastore(localLinkState, link.Attrs().Name)
	if err != nil {
		grpcOvrlyLogger.Errorf("TriggeredRemoteLinkStateUpdate: Could not update link state temporarily to %s for interface %s to K8S datastore, err %v",
			localLinkState, link.Attrs().Name, err)
		return err
	}

	switch linkState {
	case linkStateEvent(mpb.LinkState_UP):
		if err = netlink.LinkSetUp(link); err != nil {
			grpcOvrlyLogger.Errorf("TriggeredRemoteLinkStateUpdate: Could not set link state up for interface %d(%s), err: %v",
				intfId, link.Attrs().Name, err)
		}
	case linkStateEvent(mpb.LinkState_DOWN):
		if err = netlink.LinkSetDown(link); err != nil {
			grpcOvrlyLogger.Errorf("TriggeredRemoteLinkStateUpdate: Could not set link state down for interface %d(%s), err: %v",
				intfId, link.Attrs().Name, err)
		}
	default:
		err = fmt.Errorf("TriggeredRemoteLinkStateUpdate: Invalid link state %s for interface %d(%s)",
			linkState, intfId, link.Attrs().Name)
	}

	// Set state to k8s datastore
	if err == nil {
		err = updateWireStatusToK8SDatastore(linkState, link.Attrs().Name)
		if err != nil {
			grpcOvrlyLogger.Errorf("TriggeredRemoteLinkStateUpdate: Could not update link state %s to K8S datastore, err %v", linkState, err)
			return err
		}
	}
	// grpcOvrlyLogger.Infof("TriggeredRemoteLinkStateUpdate-done: Updating link state %s on interface %d", linkState, intfId)

	return err
}
