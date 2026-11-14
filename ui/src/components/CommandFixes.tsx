import CopyButton from "./CopyButton";

export interface CommandFix {
  code: string;
  commands: string[];
}

/**
 * CommandFixes lists fixes the server generated, each with its commands shown
 * and copied exactly as received. The client only words each fix's code.
 */
export default function CommandFixes({
  fixes,
  describe,
  copyLabel,
  copiedLabel,
  manualCopyTitle,
}: {
  fixes: CommandFix[];
  describe: (code: string) => string;
  copyLabel: string;
  copiedLabel: string;
  manualCopyTitle: string;
}) {
  return (
    <>
      {fixes.map((fix) => {
        const commands = fix.commands.join("\n");
        return (
          <div key={fix.code} data-fix={fix.code}>
            <p className="mb-1">{describe(fix.code)}</p>
            <div className="flex items-start gap-1">
              <pre tabIndex={0} aria-label={fix.code} className="m-0 flex-1 overflow-x-auto px-3 py-2">
                {commands}
              </pre>
              <CopyButton text={commands} label={copyLabel} success={copiedLabel} manualTitle={manualCopyTitle} size="small" />
            </div>
          </div>
        );
      })}
    </>
  );
}
