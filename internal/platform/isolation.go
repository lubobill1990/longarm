package platform

import "github.com/lubobill1990/longarm/internal/platform/ptypes"

// Account is a local OS account workloads run as (per-user isolation,
// docs/security.md). ContainerOptions.RunAs starts a container's processes
// as one.
type Account = ptypes.Account
