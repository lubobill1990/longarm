//go:build !windows

package desktop

import pb "longarm/proto/longarm/v1"

func mayBeCloudVM() bool { return false }

func localInfo() *pb.DesktopInfo {
	return &pb.DesktopInfo{Reason: "longarm desktop supports Windows workers only"}
}

func readDevBoxFacts() devBoxFacts { return devBoxFacts{} }
