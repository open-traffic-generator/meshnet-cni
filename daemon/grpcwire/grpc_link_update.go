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

type linkStateType int

func (ls linkStateType) String() string {
	switch ls {
	case linkStateType(mpb.LinkState_DOWN):
		return "down"
	case linkStateType(mpb.LinkState_UP):
		return "up"
	}
	return "unknown"
}

// --------------------------------------------------------------------------------------------------------------
// HandleGRPCLinkStateChange
//
// - find wirestatus from K8S data store for the given local node interface name for the local node
// - extract corresponding peer node ip and interface id on the peer node
// - set given link state on the interface of peer node.
// - update link state for this wirestatus in K8S datastore
func HandleGRPCLinkStateChange(link netlink.Link, ls int32) error {
	linkState := linkStateType(ls)
	intfName := link.Attrs().Name
	grpcOvrlyLogger.Infof("HandleGRPCLinkStateChange: Handle link update for interface %s to set link state to %s",
		intfName, linkState)
	// get wire status from K8S datastore
	wireStatus, err := getWireStatusFromK8SDatastore(intfName)
	if err != nil {
		grpcOvrlyLogger.Errorf("HandleGRPCLinkStateChange: err %v", err)
		return err
	}

	// // if this is repeated link state update then ignore it
	// if linkState == int32(mpb.LinkState_DOWN) && wireStatus.LinkState == int64(mpb.LinkState_DOWN_UPDATING) {
	// 	grpcOvrlyLogger.Infof("HandleGRPCLinkStateChange: Ignoring repeated link state down instruction on interface %s", intfName)
	// 	return nil
	// } else if linkState == int32(mpb.LinkState_UP) && wireStatus.LinkState == int64(mpb.LinkState_UP_UPDATING) {
	// 	grpcOvrlyLogger.Infof("HandleGRPCLinkStateChange: Ignoring repeated link state up instruction on interface %s", intfName)
	// 	return nil
	// }

	if linkState == linkStateType(mpb.LinkState_UP) {
		// +++TBD: need linux documentation why need to set node interface up when pod interface is set up??
		if err := netlink.LinkSetUp(link); err != nil {
			grpcOvrlyLogger.Errorf("HandleGRPCLinkStateChange: Could not set interface %s up in node, err %v",
				intfName, err)
		}
	}

	// set link state on the interface of peer node
	err = updateGRPCLinkStateOnPeerNodeIntf(linkState, wireStatus.WireIfaceIdOnPeerNode, wireStatus.GWirePeerNodeIp)
	if err != nil {
		grpcOvrlyLogger.Errorf("HandleGRPCLinkStateChange: Could not update link state on peer node interface, err %v", err)
		return err
	}

	// update link state in the K8S datastore
	err = updateWireStatusToK8SDatastore(linkState, wireStatus.WireIfaceNameOnLocalNode)
	if err == nil {
		grpcOvrlyLogger.Infof("HandleGRPCLinkStateChange: Successfully updated link state %s for interface %s to K8S datastore",
			linkState, intfName)
	} else {
		grpcOvrlyLogger.Errorf("HandleGRPCLinkStateChange: Could not update link state %s for interface %s to K8S datastore, err %v",
			linkState, intfName, err)
		return err
	}
	grpcOvrlyLogger.Infof("HandleGRPCLinkStateChange-done: Handle link update for interface %s to set link state to %s",
		intfName, linkState)
	return nil
}

// --------------------------------------------------------------------------------------------------------------
// getWireStatusFromK8SDatastore gets GWireStatus for the given interface 'intfName' on the local node. 'intfName'
// is unique on this node.
func getWireStatusFromK8SDatastore(intfName string) (*grpcwirev1.GWireStatus, error) {
	var wireStatus grpcwirev1.GWireStatus
	nodeName, err := findNodeName()
	if err != nil {
		grpcOvrlyLogger.Errorf("getWireStatusFromK8SDatastore: could not get node: %v", err)
		return &wireStatus, err
	}

	ctx := context.Background()
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// retrieve list of grpc wire obj list for all namespaces for the current node-name
		gwireKObjList, err := gWClient.GetWireObjListUS(ctx, nodeName)
		if err != nil {
			grpcOvrlyLogger.Errorf("getWireStatusFromK8SDatastore: could not get gWireKObjs from k8s: %v", err)
			return err
		}
		// in the unlikely situation where one has multiple topologies running in the same cluster,
		// gwireKObjList will have multiple items for this node.
		// {(<node-1><topo-namespace-1>),(<node-1><topo-namespace-2>),...}
		for _, node := range gwireKObjList.Items {
			// a node is found and node-Status-GWireKItems exists, so reconcile
			grpcWireItems, found, err := unstructured.NestedSlice(node.Object, kStatus, kGrpcWireItems)
			if err != nil {
				grpcOvrlyLogger.Errorf("getWireStatusFromK8SDatastore: could not retrieve grpcWireItem: %v", err)
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
					grpcOvrlyLogger.Errorf("getWireStatusFromK8SDatastore: unable to retrieve wire status item, %v is not a map", grpcWireItem)
					continue
				}

				// create the wire structure from the saved data in K8S datastore
				wireStatus = grpcwirev1.GWireStatus{}
				if err := apiruntime.DefaultUnstructuredConverter.FromUnstructured(wireStatusItem, &wireStatus); err != nil {
					grpcOvrlyLogger.Errorf("getWireStatusFromK8SDatastore: unable to retrieve wire status: %v", err)
					continue
				}
				if intfName == wireStatus.WireIfaceNameOnLocalNode { // this iface name is unique in a node
					return nil
				}
			}
		}
		return fmt.Errorf("getWireStatusFromK8SDatastore: interface %s not present in K8S datastore for node %s", intfName, nodeName)
	})
	return &wireStatus, retryErr
}

// --------------------------------------------------------------------------------------------------------------
// updateWireStatusToK8SDatastore updates given link state 'linkState' for the given interface 'intfName' for the
// local node in K8S datastore.
func updateWireStatusToK8SDatastore(linkState linkStateType, intfName string) error {
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
			grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: could not get gWireKObjs from k8s: %v", err)
			return err
		}
		// in the unlikely situation where one has multiple topologies running in the same cluster,
		// gwireKObjList will have multiple items for this node.
		// {(<node-1><topo-namespace-1>),(<node-1><topo-namespace-2>),...}
		for _, node := range gwireKObjList.Items {
			// a node is found and node-Status-GWireKItems exists, so reconcile
			grpcWireItems, found, err := unstructured.NestedSlice(node.Object, kStatus, kGrpcWireItems)
			if err != nil {
				grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: Could not retrieve grpcWireItem: %v", err)
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
					grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: Unable to retrieve wire status item, %v is not a map", grpcWireItem)
					continue
				}

				// create the wire structure from the saved data in K8S data store
				wireStatus = grpcwirev1.GWireStatus{}
				if err := apiruntime.DefaultUnstructuredConverter.FromUnstructured(wireStatusItem, &wireStatus); err != nil {
					grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: Unable to retrieve wire status: %v", err)
					continue
				}
				if intfName == wireStatus.WireIfaceNameOnLocalNode { // this iface name is unique in a node
					wireFound = true
					break
				}
			}

			if wireFound == true {
				// Update K8S data store

				// update linkState in wireStatusItem
				if err := unstructured.SetNestedField(grpcWireItems[wireStatusIndex].(map[string]interface{}), int64(linkState), kLinkState); err != nil {
					grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: Could not set linkstate %s for interface %s in wireStatusItem in K8S datastore, err %v",
						linkState, intfName, err)
					return err
				}
				if err := unstructured.SetNestedField(node.Object, grpcWireItems, kStatus, kGrpcWireItems); err != nil {
					grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore: Could not set grpcWireItems in K8S datastore, err %v", err)
					return err
				}

				_, err = gWClient.UpdateWireObj(ctx, wireStatus.TopoNamespace, &node)
				if err != nil {
					// grpcOvrlyLogger.Errorf("updateWireStatusToK8SDatastore (id=%d): Could not update link state %s for interface %s for node %s into K8s, err %v",
					// 	id, linkState, intfName, node.GetName(), err)
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

// ---------------------------------------------------------------------------------------------------------------
// updateGRPCLinkStateOnPeerNodeIntf sets given link state 'linkState' on the interface identified by 'peerIntfId'
// on peer node 'peerNodeIp'. It attempts 'linkStateUpdateRetryCount' times at an interval of
// 'linkStateUpdateRetryInterval' msec to set this link state.
func updateGRPCLinkStateOnPeerNodeIntf(linkState linkStateType, peerIntfId int64, peerNodeIp string) error {
	lsMsg := &mpb.LinkStateMessage{
		LinkState:  int32(linkState),
		PeerIntfId: peerIntfId,
	}

	// +++TBD: should we make this channel persistent??
	url := strings.TrimSpace(fmt.Sprintf("%s:%d", peerNodeIp, wireutil.GRPCDefaultPort))
	remote, err := grpc.Dial(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		grpcOvrlyLogger.Errorf("updateGRPCLinkStateOnPeerNodeIntf: Failed to connect to remote %s", url)
		return err
	}
	grpcOvrlyLogger.Infof("updateGRPCLinkStateOnPeerNodeIntf: Successfully connected to remote %s", url)
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

	grpcOvrlyLogger.Infof("updateGRPCLinkStateOnPeerNodeIntf: Successfully updated link state %s on peer node interface %d",
		linkState, peerIntfId)
	return nil
}

// --------------------------------------------------------------------------------------------------------------
// syncLinkStateWithPeer
//
//   - check if there is discrepancy of link state between given wire status stored in K8S datastore and
//     interface present local node
//   - if discrepancy is present then link state of interface (not of wire status) is set on the corresponding
//     interface on peer node
//   - link state is updated in K8S data store on successful update of link state on peer node
func syncLinkStateWithPeer(wireStatus grpcwirev1.GWireStatus) error {
	var (
		linkState           linkStateType = 0
		updateLinkStateInDs               = false
	)
	// check if sync is required for this local interface
	link, err := netlink.LinkByName(wireStatus.WireIfaceNameOnLocalNode)
	if err != nil {
		grpcOvrlyLogger.Errorf("syncLinkStateWithPeer: Interface %s does not exist on local node, err %v",
			wireStatus.WireIfaceNameOnLocalNode, err)
		return err
	}
	if link.Attrs().OperState == netlink.OperUp && wireStatus.LinkState == int64(mpb.LinkState_DOWN) {
		// local interface is up and wirestatus is down, sync remote to up
		linkState = linkStateType(mpb.LinkState_UP)
		grpcOvrlyLogger.Infof("syncLinkStateWithPeer: Syncing remote wire interface (%d) to up because local"+
			" interface (%s) is up but wirestatus is down",
			wireStatus.WireIfaceIdOnPeerNode, wireStatus.WireIfaceNameOnLocalNode)
		err = updateGRPCLinkStateOnPeerNodeIntf(linkState, wireStatus.WireIfaceIdOnPeerNode, wireStatus.GWirePeerNodeIp)
		updateLinkStateInDs = true
	} else if link.Attrs().OperState == netlink.OperLowerLayerDown && wireStatus.LinkState == int64(mpb.LinkState_UP) {
		// local interface is down and wirestatus is up, sync remote to down
		linkState = linkStateType(mpb.LinkState_DOWN)
		grpcOvrlyLogger.Infof("syncLinkStateWithPeer: Syncing remote wire interface (%d) to down because local"+
			" interface (%s) is down but wirestatus is up",
			wireStatus.WireIfaceIdOnPeerNode, wireStatus.WireIfaceNameOnLocalNode)
		err = updateGRPCLinkStateOnPeerNodeIntf(linkState, wireStatus.WireIfaceIdOnPeerNode, wireStatus.GWirePeerNodeIp)
		updateLinkStateInDs = true
	}
	if err != nil {
		grpcOvrlyLogger.Errorf("syncLinkStateWithPeer: Could not update link state on peer node interface (%d), err %v",
			wireStatus.WireIfaceIdOnPeerNode, err)
		return err
	}
	if updateLinkStateInDs {
		err = updateWireStatusToK8SDatastore(linkState, wireStatus.WireIfaceNameOnLocalNode)
		if err != nil {
			grpcOvrlyLogger.Errorf("syncLinkStateWithPeer: Could not update link state %s to K8S datastore, err %v", linkState, err)
			return err
		}
	}

	return nil
}

// --------------------------------------------------------------------------------------------------------------
// TriggeredRemoteLinkStateUpdate sets given link state 'linkState' on the interface identified by id 'intfId'
// on local node.
func TriggeredRemoteLinkStateUpdate(ls int32, intfId int64) error {
	linkState := linkStateType(ls)
	// grpcOvrlyLogger.Infof("TriggeredRemoteLinkStateUpdate: Updating link state %s on interface %d",
	// 	linkState, intfId)

	link, err := netlink.LinkByIndex(int(intfId))
	if err != nil {
		grpcOvrlyLogger.Errorf("TriggeredRemoteLinkStateUpdate: Could not retrieve interface %d, err %v", intfId, err)
		return err
	}

	grpcOvrlyLogger.Infof("TriggeredRemoteLinkStateUpdate: Updating link state %s on interface %s",
		linkState, link.Attrs().Name)

	// // update link state to updating in k8s datastore so that subsequent repeated changes because of following
	// // up/down instruction can be avoided. The intended state will be set after successful operation.
	// localLinkState := int32(mpb.LinkState_UP_UPDATING)
	// if linkState == int32(mpb.LinkState_DOWN) {
	// 	localLinkState = int32(mpb.LinkState_DOWN_UPDATING)
	// }
	// err = updateWireStatusToK8SDatastore(linkState, link.Attrs().Name)
	// if err == nil {
	// 	grpcOvrlyLogger.Infof("TriggeredRemoteLinkStateUpdate: Updating link state temporarily to %d for interface %s to K8S datastore",
	// 		linkState, link.Attrs().Name)
	// } else {
	// 	grpcOvrlyLogger.Errorf("TriggeredRemoteLinkStateUpdate: Could not update link state temporarily to %d for interface %s to K8S datastore, err %v",
	// 		linkState, link.Attrs().Name, err)
	// 	return err
	// }

	switch linkState {
	case linkStateType(mpb.LinkState_UP):
		if err = netlink.LinkSetUp(link); err != nil {
			grpcOvrlyLogger.Errorf("TriggeredRemoteLinkStateUpdate: Could not set link up for interface %d(%s), err %v",
				intfId, link.Attrs().Name, err)
		}
	case linkStateType(mpb.LinkState_DOWN):
		if err = netlink.LinkSetDown(link); err != nil {
			grpcOvrlyLogger.Errorf("TriggeredRemoteLinkStateUpdate: Could not set link down for interface %d(%s), err %v",
				intfId, link.Attrs().Name, err)
		}
	default:
		err = fmt.Errorf("TriggeredRemoteLinkStateUpdate: Invalid link state type %s for interface %d(%s)",
			linkState, intfId, link.Attrs().Name)
	}

	// Set state to k8s datastore
	if err == nil {
		err = updateWireStatusToK8SDatastore(linkState, link.Attrs().Name)
		if err == nil {
			grpcOvrlyLogger.Infof("TriggeredRemoteLinkStateUpdate: Successfully updated link state %s for interface %s to K8S datastore",
				linkState, link.Attrs().Name)
		} else {
			grpcOvrlyLogger.Errorf("TriggeredRemoteLinkStateUpdate: Could not update link state %s to K8S datastore, err %v", linkState, err)
			return err
		}
	}
	grpcOvrlyLogger.Infof("TriggeredRemoteLinkStateUpdate-done: Updating link state %s on interface %d",
		linkState, intfId)

	return err
}
