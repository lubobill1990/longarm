//go:build !windows

package desktop

import pb "github.com/lubobill1990/longarm/proto/longarm/v1"

func mayBeCloudVM() bool { return false }

func localInfo() *pb.DesktopInfo {
	return &pb.DesktopInfo{Reason: "longarm desktop supports Windows workers only"}
}
