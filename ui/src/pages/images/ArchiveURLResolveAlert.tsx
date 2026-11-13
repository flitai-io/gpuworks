import AlertCircleIcon from "@hugeicons/core-free-icons/AlertCircleIcon";
import { Alert, AlertDescription, AlertIcon, AlertTitle } from "../../components/ui/alert";
import { createHugeIcon } from "../../lib/huge-icon";
import { useTranslation } from "react-i18next";
import { APIError, apiErrorText } from "../../apiError";

const ResolveAlertIcon = createHugeIcon(AlertCircleIcon);

type Props = {
  error: Error;
  fallback: string;
};

/**
 * ArchiveURLResolveAlert shows one archive resolve failure.
 * Allowing a host is a checkbox on the address field, not a second action here.
 */
export default function ArchiveURLResolveAlert({ error, fallback }: Props) {
  const { t: tCommon } = useTranslation("common");
  const message = error instanceof APIError
    ? apiErrorText((key, options) => tCommon(key, { ...options, defaultValue: fallback }), error)
    : error.message;
  const blocked = error instanceof APIError && (error.code === "IMAGE_ARCHIVE_URL_POLICY_DENIED" || error.code === "IMAGE_ARCHIVE_URL_POLICY_BLOCKED");
  return (
    <Alert status={blocked ? "danger" : "warning"} role="alert">
      <AlertIcon><ResolveAlertIcon /></AlertIcon>
      <AlertTitle>{message}</AlertTitle>
      {error instanceof APIError && <AlertDescription>HTTP {error.status} · {error.code}</AlertDescription>}
    </Alert>
  );
}
