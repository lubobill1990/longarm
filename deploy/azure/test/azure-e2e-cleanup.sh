#!/usr/bin/env bash
# azure-e2e-cleanup.sh: delete what azure-e2e.sh's deploy created in a shared
# resource group (the group itself stays: CI may only touch its contents).
#
#   azure-e2e-cleanup.sh RG NAME          one hub (everything tagged longarm-hub=NAME)
#   azure-e2e-cleanup.sh RG --older 6h    every e2e hub (lae2e*) older than that (leftovers)
set -uo pipefail

rg=${1:?resource group}
shift

# delete_hub NAME: role assignments of its identity first (they outlive
# the identity as orphans), then the container group, then the rest
delete_hub() {
	local name=$1 pid
	echo "cleanup: $name"
	pid=$(az identity show -g "$rg" -n "$name-id" --query principalId -o tsv 2>/dev/null | tr -d '\r')
	if [[ -n $pid ]]; then
		az role assignment delete --assignee "$pid" --resource-group "$rg" --yes 2>/dev/null || true
		az role assignment list --all --assignee "$pid" --query "[].id" -o tsv 2>/dev/null | tr -d '\r' |
			while read -r id; do az role assignment delete --ids "$id" 2>/dev/null || true; done
	fi
	az container delete -g "$rg" -n "$name" --yes -o none 2>/dev/null || true
	az resource list -g "$rg" --tag "longarm-hub=$name" --query "[].id" -o tsv 2>/dev/null | tr -d '\r' |
		while read -r id; do az resource delete --ids "$id" -o none 2>/dev/null || echo "cleanup: could not delete $id"; done
}

if [[ ${1:-} == --older ]]; then
	age=${2:?duration like 6h}
	cutoff=$(date -u -d "-${age%h} hours" +%Y-%m-%dT%H:%M:%SZ)
	az resource list -g "$rg" --resource-type Microsoft.ContainerInstance/containerGroups \
		--query "[?starts_with(name, 'lae2e') && createdTime < '$cutoff'].name" -o tsv 2>/dev/null | tr -d '\r' |
		while read -r n; do [[ -z $n ]] || delete_hub "$n"; done
	# identities / storage of hubs whose container group is already gone
	az resource list -g "$rg" --query "[?tags.\"longarm-hub\" != null && starts_with(tags.\"longarm-hub\", 'lae2e') && createdTime < '$cutoff'].tags.\"longarm-hub\"" -o tsv 2>/dev/null |
		tr -d '\r' | sort -u | while read -r n; do [[ -z $n ]] || delete_hub "$n"; done
else
	delete_hub "${1:?hub name}"
fi
