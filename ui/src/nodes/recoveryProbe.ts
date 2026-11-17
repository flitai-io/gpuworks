import { useState } from "react";
import { message } from "antd";
import { useTranslation } from "react-i18next";

import { api, type Node } from "../api";
import { recoveryErrorText, recoverySkippedText } from "./recoveryError";

/**
 * Asking the platform to bring a node's agent back over SSH, as one action.
 * The reconnect button beside the connection badge is the one entry point;
 * the call and its messages live here so a second one would report the same
 * outcomes in the same words.
 */
export function useRecoveryProbe(node: Pick<Node, "uuid">, onUpdated: (node: Node) => void) {
  const { t } = useTranslation("nodes");
  const [loading, setLoading] = useState(false);

  const probe = async () => {
    setLoading(true);
    try {
      const outcome = await api.probeNodeRecovery(node.uuid);
      onUpdated(await api.node(node.uuid));
      if (outcome.recovered) message.success(t("recovery.message.recovered"));
      else if (outcome.skipped_code || outcome.skipped_reason) message.info(recoverySkippedText(t, outcome.skipped_code, outcome.skipped_reason));
      else if (outcome.error) message.warning(recoveryErrorText(t, outcome.error_code, outcome.error));
      else message.success(t("recovery.message.probeComplete"));
    } catch (error) {
      message.error(t("recovery.message.probeFailed", { detail: (error as Error).message }));
    } finally {
      setLoading(false);
    }
  };

  return { loading, probe };
}
