import { useTranslation } from "react-i18next";
import { canWriteStorage, type Storage, type StorageModelFile } from "../../storageTypes";
import { ModelNodeFileDelete } from "../models/ModelNodeFileDelete";
import { ModelStorageFileDelete } from "../models/ModelStorageFileDelete";

/**
 * What an admin can do to one row: the shared copy is deleted from the storage
 * (with the "other gpuworks use it" check); a node's copy or cache is removed
 * from that node only.
 */
export default function StorageFileActions({ storage, file, files, onChanged }: {
  storage: Storage;
  file: StorageModelFile;
  /** All rows, to count how many node caches a shared copy feeds. */
  files: StorageModelFile[];
  onChanged: () => void;
}) {
  const { t: tStorage } = useTranslation("storage");
  const writable = canWriteStorage(storage);
  if (!file.node_uuid) {
    return (
      <ModelStorageFileDelete
        modelId={file.model_id}
        storageId={storage.id}
        storageName={storage.name}
        storageType={storage.type}
        writable={writable}
        cachedNodes={files.filter((f) => f.cache && f.model_id === file.model_id).length}
        onDone={onChanged}
      />
    );
  }
  return (
    <ModelNodeFileDelete
      modelId={file.model_id}
      nodeUuid={file.node_uuid}
      nodeName={file.node_name}
      storageId={storage.id}
      storageName={storage.name}
      cache={file.cache}
      // A node cache is not the storage's library: removing it is allowed on
      // a disabled or read-only storage too, as the server allows.
      writable={file.cache || writable}
      label={file.cache ? tStorage("files.evictCache") : undefined}
      onDone={onChanged}
    />
  );
}
