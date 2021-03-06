// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package topo

import (
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/youtube/vitess/go/vt/key"
)

// Functions for dealing with shard representations in topology.

// SourceShard represents a data source for filtered replication
// accross shards. When this is used in a destination shard, the master
// of that shard will run filtered replication.
type SourceShard struct {
	// Uid is the unique ID for this SourceShard object.
	// It is for instance used as a unique index in blp_checkpoint
	// when storing the position. It should be unique whithin a
	// destination Shard, but not globally unique.
	Uid uint32

	// the source keyspace
	Keyspace string

	// the source shard
	Shard string

	// the source shard keyrange
	KeyRange key.KeyRange

	// we could add other filtering information, like table list, ...
}

func (source *SourceShard) String() string {
	return fmt.Sprintf("SourceShard(%v,%v/%v)", source.Uid, source.Keyspace, source.Shard)
}

// A pure data struct for information stored in topology server.  This
// node is used to present a controlled view of the shard, unaware of
// every management action. It also contains configuration data for a
// shard.
type Shard struct {
	// There can be only at most one master, but there may be none. (0)
	MasterAlias TabletAlias

	// This must match the shard name based on our other conventions, but
	// helpful to have it decomposed here.
	KeyRange key.KeyRange

	// ServedTypes is a list of all the tablet types this shard will
	// serve. This is usually used with overlapping shards during
	// data shuffles like shard splitting.
	ServedTypes []TabletType

	// SourceShards is the list of shards we're replicating from,
	// using filtered replication.
	SourceShards []SourceShard

	// Cells is the list of cells that have tablets for this shard.
	// It is populated at InitTablet time when a tabelt is added
	// in a cell that is not in the list yet.
	Cells []string
}

func newShard() *Shard {
	return &Shard{}
}

// ValidateShardName takes a shard name and sanitizes it, and also returns
// the KeyRange.
func ValidateShardName(shard string) (string, key.KeyRange, error) {
	if !strings.Contains(shard, "-") {
		return shard, key.KeyRange{}, nil
	}

	parts := strings.Split(shard, "-")
	if len(parts) != 2 {
		return "", key.KeyRange{}, fmt.Errorf("Invalid shardId, can only contain one '-': %v", shard)
	}

	keyRange, err := key.ParseKeyRangeParts(parts[0], parts[1])
	if err != nil {
		return "", key.KeyRange{}, err
	}

	if keyRange.End != key.MaxKey && keyRange.Start >= keyRange.End {
		return "", key.KeyRange{}, fmt.Errorf("Out of order keys: %v is not strictly smaller than %v", keyRange.Start.Hex(), keyRange.End.Hex())
	}

	return strings.ToUpper(shard), keyRange, nil
}

// HasCell returns true if the cell is listed in the Cells for the shard.
func (shard *Shard) HasCell(cell string) bool {
	for _, c := range shard.Cells {
		if c == cell {
			return true
		}
	}
	return false
}

// ShardInfo is a meta struct that contains metadata to give the data
// more context and convenience. This is the main way we interact with a shard.
type ShardInfo struct {
	keyspace  string
	shardName string
	*Shard
}

// Keyspace returns the keyspace a shard belongs to
func (si *ShardInfo) Keyspace() string {
	return si.keyspace
}

// ShardName returns the shard name for a shard
func (si *ShardInfo) ShardName() string {
	return si.shardName
}

// Rebuild takes all the tablets in the list and puts them in the
// right place in the shard. Only master tablet is considered.
func (si *ShardInfo) Rebuild(shardTablets []*TabletInfo) error {
	si.MasterAlias = TabletAlias{}
	si.KeyRange = key.KeyRange{}

	for i, ti := range shardTablets {
		switch ti.Type {
		case TYPE_MASTER:
			si.MasterAlias = ti.Alias()
		}

		if i == 0 {
			// copy the first KeyRange
			si.KeyRange = ti.KeyRange
		} else {
			// verify the subsequent ones
			if si.KeyRange != ti.KeyRange {
				return fmt.Errorf("inconsistent KeyRange: %v != %v", si.KeyRange, ti.KeyRange)
			}
		}
	}
	return nil
}

// NewShardInfo returns a ShardInfo basing on shard with the
// keyspace / shard. This function should be only used by Server
// implementations.
func NewShardInfo(keyspace, shard string, value *Shard) *ShardInfo {
	return &ShardInfo{
		keyspace:  keyspace,
		shardName: shard,
		Shard:     value,
	}
}

// CreateShard creates a new shard and tries to fill in the right information.
func CreateShard(ts Server, keyspace, shard string) error {

	name, keyRange, err := ValidateShardName(shard)
	if err != nil {
		return err
	}
	s := &Shard{KeyRange: keyRange}

	// start the shard with all serving types. If it overlaps with
	// other shards for some serving types, remove them.
	servingTypes := map[TabletType]bool{
		TYPE_MASTER:  true,
		TYPE_REPLICA: true,
		TYPE_RDONLY:  true,
	}
	sis, err := FindAllShardsInKeyspace(ts, keyspace)
	if err != nil && err != ErrNoNode {
		return err
	}
	for _, si := range sis {
		if key.KeyRangesIntersect(si.KeyRange, keyRange) {
			for _, t := range si.ServedTypes {
				delete(servingTypes, t)
			}
		}
	}
	s.ServedTypes = make([]TabletType, 0, len(servingTypes))
	for st, _ := range servingTypes {
		s.ServedTypes = append(s.ServedTypes, st)
	}

	return ts.CreateShard(keyspace, name, s)
}

func tabletAliasesRecursive(ts Server, keyspace, shard, repPath string) ([]TabletAlias, error) {
	mutex := sync.Mutex{}
	wg := sync.WaitGroup{}
	result := make([]TabletAlias, 0, 32)
	children, err := ts.GetReplicationPaths(keyspace, shard, repPath)
	if err != nil {
		return nil, err
	}

	for _, child := range children {
		wg.Add(1)
		go func(child TabletAlias) {
			childPath := path.Join(repPath, child.String())
			rChildren, subErr := tabletAliasesRecursive(ts, keyspace, shard, childPath)
			if subErr != nil {
				// If other processes are deleting
				// nodes, we need to ignore the
				// missing nodes.
				if subErr != ErrNoNode {
					mutex.Lock()
					err = subErr
					mutex.Unlock()
				}
			} else {
				mutex.Lock()
				result = append(result, child)
				for _, rChild := range rChildren {
					result = append(result, rChild)
				}
				mutex.Unlock()
			}
			wg.Done()
		}(child)
	}

	wg.Wait()
	if err != nil {
		return nil, err
	}
	return result, nil
}

// FindAllTabletAliasesInShard uses the replication graph to find all the
// tablet aliases in the given shard.
func FindAllTabletAliasesInShard(ts Server, keyspace, shard string) ([]TabletAlias, error) {
	return tabletAliasesRecursive(ts, keyspace, shard, "")
}
