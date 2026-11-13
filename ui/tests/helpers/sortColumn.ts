/**
 * Sortable column titles open Zeron's sort menu (Ascending / Descending /
 * Clear sorting) instead of cycling on click; this picks one of its entries.
 * Testing Library is loaded on call: test files set up the DOM globals first.
 */
export async function sortColumn(name: string | RegExp, choice: "Ascending" | "Descending" | "Clear sorting") {
  const { fireEvent, screen } = await import("@testing-library/react");
  fireEvent.click(screen.getByRole("button", { name }));
  fireEvent.click(await screen.findByText(choice));
}
