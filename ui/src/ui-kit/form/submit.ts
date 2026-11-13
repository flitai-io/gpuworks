import type { FieldErrors, FieldValues, SubmitErrorHandler, SubmitHandler, UseFormReturn } from "react-hook-form";

function errorAt(errors: FieldErrors, name: string): unknown {
  return name.replace(/\[(\d+)\]/g, ".$1").split(".").reduce<unknown>((value, key) => (
    value && typeof value === "object" ? (value as Record<string, unknown>)[key] : undefined
  ), errors);
}

/** DOM order, scoped to this form, including nested/array names and controls in popups. */
export function scrollToFirstError(root: HTMLElement | null, errors: FieldErrors) {
  const first = [...(root?.querySelectorAll<HTMLElement>("[data-form-field]") ?? [])]
    .find((element) => errorAt(errors, element.dataset.formField ?? ""));
  if (!first) return;
  first.scrollIntoView?.({ block: "center", behavior: "smooth" });
  first.querySelector<HTMLElement>('input:not([disabled]),textarea:not([disabled]),button:not([disabled]),select:not([disabled]),[tabindex="0"]')?.focus({ preventScroll: true });
}

/** Use shouldFocusError:false with useForm so focus and scrolling have one owner. */
export function formSubmit<T extends FieldValues>(
  form: Pick<UseFormReturn<T>, "handleSubmit">,
  root: () => HTMLElement | null,
  onValid: SubmitHandler<T>,
  onInvalid?: SubmitErrorHandler<T>,
) {
  return form.handleSubmit(onValid, (errors, event) => {
    scrollToFirstError(root(), errors);
    return onInvalid?.(errors, event);
  });
}
