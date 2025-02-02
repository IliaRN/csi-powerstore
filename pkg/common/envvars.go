/*
 *
 * Copyright © 2021 Dell Inc. or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package common

const (
	// EnvDriverName is the name of the csi driver (provisioner)
	EnvDriverName = "X_CSI_DRIVER_NAME"

	// EnvNodeIDFilePath is the name of the environment variable used to
	// specify the file with the node ID
	EnvNodeIDFilePath = "X_CSI_POWERSTORE_NODE_ID_PATH"

	// EnvKubeNodeName is the name of the environment variable which stores current kubernetes
	// node name
	EnvKubeNodeName = "X_CSI_POWERSTORE_KUBE_NODE_NAME"

	// EnvNodeNamePrefix is the name of the environment variable which stores prefix which will be
	// used when registering node on PowerStore array
	EnvNodeNamePrefix = "X_CSI_POWERSTORE_NODE_NAME_PREFIX"

	// EnvNodeChrootPath is the name of the environment variable which store path to chroot where
	// to execute iSCSI commands
	EnvNodeChrootPath = "X_CSI_POWERSTORE_NODE_CHROOT_PATH"

	// EnvTmpDir is the name of the environment variable which store path to the folder which will be used
	// for csi-powerstore temporary files
	EnvTmpDir = "X_CSI_POWERSTORE_TMP_DIR"

	// EnvFCPortsFilterFilePath is the name of the environment variable which store path to the file which
	// provide list of WWPN which should be used by the driver for FC connection on this node
	// example:
	// content of the file:
	//   21:00:00:29:ff:48:9f:6e,21:00:00:29:ff:48:9f:6e
	// If file not exist or empty or in invalid format, then the driver will use all available FC ports
	EnvFCPortsFilterFilePath = "X_CSI_FC_PORTS_FILTER_FILE_PATH"

	// EnvThrottlingRateLimit sets a number of concurrent requests to APi
	EnvThrottlingRateLimit = "X_CSI_POWERSTORE_THROTTLING_RATE_LIMIT"

	// EnvEnableCHAP is the flag which determines if the driver is going
	// to set the CHAP credentials in the ISCSI node database at the time
	// of node plugin boot
	EnvEnableCHAP = "X_CSI_POWERSTORE_ENABLE_CHAP"

	// EnvExternalAccess is the IP of an additional router you wish to add for nfs export
	// Used to provide NFS volumes behind NAT
	EnvExternalAccess = "X_CSI_POWERSTORE_EXTERNAL_ACCESS" // #nosec G101

	// EnvArrayConfigFilePath is filepath to powerstore arrays config file
	EnvArrayConfigFilePath = "X_CSI_POWERSTORE_CONFIG_PATH"

	// EnvConfigParamsFilePath is filepath to powerstore driver params config file
	EnvConfigParamsFilePath = "X_CSI_POWERSTORE_CONFIG_PARAMS_PATH"

	// EnvDebugEnableTracing allow to enable tracing in driver
	EnvDebugEnableTracing = "ENABLE_TRACING"

	// EnvReplicationContextPrefix enables sidecars to read required information from volume context
	EnvReplicationContextPrefix = "X_CSI_REPLICATION_CONTEXT_PREFIX"

	// EnvReplicationPrefix is used as a prefix to find out if replication is enabled
	EnvReplicationPrefix = "X_CSI_REPLICATION_PREFIX"

	// EnvGOCSIDebug indicates whether to print REQUESTs and RESPONSEs of all CSI method calls(from gocsi)
	EnvGOCSIDebug = "X_CSI_DEBUG"
)
