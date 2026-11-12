package messages

// ErrorStorageCacheNotEmpty refuses switching an nfs_cache storage to nfs
// while node caches still hold entries ({nodes} nodes, {entries} entries,
// {transferring} still transferring); clear the caches first.
const ErrorStorageCacheNotEmpty = "STORAGE_CACHE_NOT_EMPTY"
