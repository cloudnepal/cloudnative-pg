/*
This file is part of Cloud Native PostgreSQL.

Copyright (C) 2019-2021 EnterpriseDB Corporation.
*/

// Package restorer manages the WAL restore process
package restorer

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"

	apiv1 "github.com/EnterpriseDB/cloud-native-postgresql/api/v1"
	barmanCapabilities "github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/barman/capabilities"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/barman/spool"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/execlog"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/log"
)

// WALRestorer is a structure containing every info needed to restore
// some WALs from the object storage
type WALRestorer struct {
	// The cluster for which we are archiving
	cluster *apiv1.Cluster

	// The spool of WAL files to be archived in parallel
	spool *spool.WALSpool

	// The environment that should be used to invoke barman-cloud-wal-archive
	env []string
}

// Result is the structure filled by the restore process on completion
type Result struct {
	// The name of the WAL file to restore
	WalName string

	// Where to store the restored WAL file
	DestinationPath string

	// If not nil, this is the error that has been detected
	Err error

	// The time when we started barman-cloud-wal-archive
	StartTime time.Time

	// The time when end barman-cloud-wal-archive ended
	EndTime time.Time
}

// New creates a new WAL archiver
func New(ctx context.Context, cluster *apiv1.Cluster, env []string, spoolDirectory string) (
	archiver *WALRestorer,
	err error,
) {
	contextLog := log.FromContext(ctx)
	var walRecoverSpool *spool.WALSpool

	if walRecoverSpool, err = spool.New(spoolDirectory); err != nil {
		contextLog.Info("Cannot initialize the WAL spool", "spoolDirectory", spoolDirectory)
		return nil, fmt.Errorf("while creating spool directory: %w", err)
	}

	archiver = &WALRestorer{
		cluster: cluster,
		spool:   walRecoverSpool,
		env:     env,
	}
	return archiver, nil
}

// RestoreFromSpool restores a certain file from the spool, returning a boolean flag indicating
// is the file was in the spool or not. If the file was in the spool, it will be moved into the
// specified destination path
func (restorer *WALRestorer) RestoreFromSpool(walName, destinationPath string) (wasInSpool bool, err error) {
	err = restorer.spool.MoveOut(walName, destinationPath)
	switch {
	case err == spool.ErrorNonExistentFile:
		return false, nil

	case err != nil:
		return false, err

	default:
		return true, nil
	}
}

// RestoreList restores a list of WALs. The first WAL of the list will go directly into the
// destination path, the others will be adopted by the spool
func (restorer *WALRestorer) RestoreList(
	ctx context.Context,
	fetchList []string,
	destinationPath string,
	options []string,
) (resultList []Result) {
	resultList = make([]Result, len(fetchList))
	contextLog := log.FromContext(ctx)
	var waitGroup sync.WaitGroup

	for idx := range fetchList {
		waitGroup.Add(1)
		go func(walIndex int) {
			result := &resultList[walIndex]
			result.WalName = fetchList[walIndex]
			if walIndex == 0 {
				// The WAL that PostgreSQL requested will go directly
				// to the destination path
				result.DestinationPath = destinationPath
			} else {
				result.DestinationPath = restorer.spool.FileName(result.WalName)
			}

			result.StartTime = time.Now()
			result.Err = restorer.Restore(fetchList[walIndex], result.DestinationPath, options)
			result.EndTime = time.Now()

			elapsedWalTime := result.EndTime.Sub(result.StartTime)
			if result.Err == nil {
				contextLog.Info(
					"Restored WAL file",
					"walName", result.WalName,
					"startTime", result.StartTime,
					"endTime", result.EndTime,
					"elapsedWalTime", elapsedWalTime)
			} else if walIndex == 0 {
				// We don't log errors for prefetched WALs but just for the
				// first WAL, which is the one requested by PostgreSQL.
				//
				// The implemented prefetch is speculative and this WAL may just
				// not exist, this means that this may not be a real error.
				contextLog.Warning(
					"Failed restoring WAL: PostgreSQL will retry if needed",
					"walName", result.WalName,
					"options", options,
					"startTime", result.StartTime,
					"endTime", result.EndTime,
					"elapsedWalTime", elapsedWalTime,
					"error", result.Err)
			}
			waitGroup.Done()
		}(idx)
	}

	waitGroup.Wait()
	return resultList
}

// Restore restores a WAL file from the object store
func (restorer *WALRestorer) Restore(walName, destinationPath string, baseOptions []string) error {
	options := make([]string, len(baseOptions), len(baseOptions)+2)
	copy(options, baseOptions)
	options = append(options, walName, destinationPath)

	barmanCloudWalRestoreCmd := exec.Command(
		barmanCapabilities.BarmanCloudWalRestore,
		options...) // #nosec G204
	barmanCloudWalRestoreCmd.Env = restorer.env
	err := execlog.RunStreaming(barmanCloudWalRestoreCmd, barmanCapabilities.BarmanCloudWalRestore)
	if err != nil {
		return fmt.Errorf("unexpected failure invoking %s: %w", barmanCapabilities.BarmanCloudWalRestore, err)
	}

	return nil
}