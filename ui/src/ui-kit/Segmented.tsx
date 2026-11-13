import { Tabs, TabsList, TabItem } from "../components/ui/tabs";
export function Segmented({ value, onChange, options, label }: {
  value: string; onChange: (value: string) => void; label: string;
  options: { value: string; label: string; disabled?: boolean }[];
}) {
  return <Tabs value={value} onValueChange={onChange} variant="segment"><TabsList aria-label={label}>
    {options.map((option) => <TabItem key={option.value} {...option} />)}
  </TabsList></Tabs>;
}
