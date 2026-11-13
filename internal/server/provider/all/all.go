// Package all blank-imports every provider driver so their init() registration
// runs. Import this once from the app assembly.
package all

import (
	_ "gpuworks.ai/gpuworks/internal/server/provider/aliyun"
	_ "gpuworks.ai/gpuworks/internal/server/provider/autodl"
	_ "gpuworks.ai/gpuworks/internal/server/provider/compshare"
	_ "gpuworks.ai/gpuworks/internal/server/provider/digitalocean"
	_ "gpuworks.ai/gpuworks/internal/server/provider/runpod"
	_ "gpuworks.ai/gpuworks/internal/server/provider/vast"
	_ "gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud"
)
