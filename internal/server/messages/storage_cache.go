package messages

// ErrorStorageCacheNotEmpty refuses switching an nfs_cache storage to nfs
// while node caches still hold entries ({entries} entries on {nodes} nodes,
// {transferring} still copying or downloading, node_uuids); clear the caches
// first.
const ErrorStorageCacheNotEmpty = "STORAGE_CACHE_NOT_EMPTY"

// ErrorStorageCacheClearFailed: clearing node caches failed on {failed}
// nodes, the first being {node}; the body also lists each failure.
const ErrorStorageCacheClearFailed = "STORAGE_CACHE_CLEAR_FAILED"
