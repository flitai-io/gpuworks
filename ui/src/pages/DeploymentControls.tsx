import { type CSSProperties, type ReactNode, type ComponentProps } from "react";
import { Badge as ZeronBadge } from "../components/ui/badge";
import { Button as ZeronButton } from "../components/ui/button";
import { Checkbox as ZeronCheckbox } from "../components/ui/checkbox";
import { NumberInput } from "../ui-kit/NumberInput";
import { Progress as Track } from "../ui-kit/Progress";
import { Notice } from "../ui-kit/Notice";
import { useTranslation } from "react-i18next";

export function Tag({color, children, ...props}: {color?:string; children?:ReactNode; className?:string; title?:string}) {
  const status=({warning:"warning",orange:"warning",green:"success",red:"danger"} as const)[color ?? ""];
  const category=({blue:"blue",geekblue:"blue",cyan:"cyan",purple:"purple"} as const)[color ?? ""];
  return <ZeronBadge size="sm" status={status} color={category} {...props}>{children}</ZeronBadge>;
}
export function Badge({status,text}: {status:string;text:ReactNode}) {
  const tone=({processing:"info",default:"neutral",warning:"warning",success:"success",error:"danger"} as const)[status];
  return <ZeronBadge size="sm" variant="plain" status={tone ?? "neutral"}>{text}</ZeronBadge>;
}
export function Alert({type="info",message,description,action,role,style,className}: {
  type?:"info"|"error"|"warning";message:ReactNode;description?:ReactNode;action?:ReactNode;role?:string;style?:CSSProperties;className?:string;showIcon?:boolean;
}) {
  return <div style={style}><Notice role={role ?? "alert"} className={className} tone={type==="error"?"danger":type} title={message} action={action}>{description}</Notice></div>;
}
export function Progress({percent=0,showInfo=true,style,label,"aria-label":ariaLabel}: {
  percent?:number;showInfo?:boolean;style?:CSSProperties;label?:string;"aria-label"?:string;
  size?:string;status?:string;strokeColor?:string;trailColor?:string;
}) {
  const { t }=useTranslation("deployments");
  return <div className="deployment-progress" style={style}><Track value={percent} label={ariaLabel ?? label ?? t("instance.progressAria")} />
    {showInfo && <span className="text-label text-fg-muted tabular-nums">{percent}%</span>}</div>;
}
export function Checkbox({checked,children,onChange,disabled}: {checked?:boolean;children:ReactNode;disabled?:boolean;onChange?:(e:{target:{checked:boolean}})=>void}) {
  return <label className="inline-flex items-center gap-2 text-body text-fg-default"><ZeronCheckbox checked={checked} disabled={disabled} onCheckedChange={v=>onChange?.({target:{checked:v}})} />{children}</label>;
}
export function InputNumber({addonBefore,addonAfter,style,size,...props}: Omit<ComponentProps<typeof NumberInput>,"size"> & {size?:string;addonBefore?:ReactNode;addonAfter?:ReactNode;style?:CSSProperties}) {
  return <span className="inline-flex max-w-full items-center gap-1 text-body" style={style}>{addonBefore}<NumberInput {...props} size="sm" className="w-full min-w-0" />{addonAfter}</span>;
}
export function Button({size,type,icon,children,...props}: Omit<ComponentProps<typeof ZeronButton>,"size"|"type"> & {size?:string;type?:string;icon?:ReactNode}) {
  return <ZeronButton size={size==="small"?"sm":"md"} variant={type==="link"?"link":type==="text"?"ghost":type==="primary"?"primary":"tertiary"} {...props}>{icon}{children}</ZeronButton>;
}
