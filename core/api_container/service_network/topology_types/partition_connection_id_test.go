/*
 * Copyright (c) 2020 - present Kurtosis Technologies LLC.
 * All Rights Reserved.
 */

package topology_types

import (
	"gotest.tools/assert"
	"testing"
)

const (
	partition1 PartitionID = "partition1"
	partition2 PartitionID = "partition2"
)

func TestCommutativeness(t *testing.T) {
	forward := *NewPartitionConnectionID(partition1, partition2)
	reverse := *NewPartitionConnectionID(partition2, partition1)

	theMap := map[PartitionConnectionID]bool{
		forward: true,
	}

	_, found := theMap[reverse]
	assert.Assert(t, found, "Expected to find reverse mapping in the map due to commutativeness")
}
