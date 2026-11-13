import { fireEvent, within } from "@testing-library/react";

export function selectInstance(uuid: string) {
  if (document.getElementById(`instance-${uuid}`)) return;
  const page = within(document.body);
  const tab = page.queryByRole("tab", { name: /^Instance details \(/ });
  if (tab && tab.getAttribute("aria-selected") !== "true") fireEvent.click(tab);
  const picker = () => page.getByRole("group", { name: "Switch instance" }).querySelector("button[role=combobox]") as HTMLButtonElement;
  const chooseRecord = () => {
    const all = page.getByRole("button", { name: /^All \d+$/ });
    fireEvent.click(all);
    const trigger = picker();
    if (trigger.disabled) return false;
    fireEvent.click(trigger);
    const option = page.queryByRole("option", { name: name => name.startsWith(uuid + " ") });
    if (option) fireEvent.click(option);
    else fireEvent.keyDown(page.getByPlaceholderText("Switch instance"), { key: "Escape" });
    return Boolean(option);
  };
  if (chooseRecord()) return;
  const replica = page.getByRole("combobox", { name: "Switch replica" });
  fireEvent.click(replica);
  const choices = page.getAllByRole("option").map(option => option.getAttribute("aria-label")!);
  for (const [index, name] of choices.entries()) {
    if (index) fireEvent.click(replica);
    fireEvent.click(page.getByRole("option", { name, exact: true }));
    if (chooseRecord()) return;
  }
  throw new Error(`Instance ${uuid} was missing from every replica`);
}
