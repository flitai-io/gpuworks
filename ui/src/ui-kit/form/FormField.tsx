import { useId, type ReactNode } from "react";
import { useController, type ControllerRenderProps, type FieldPath, type FieldValues, type UseControllerProps } from "react-hook-form";
import { Field, FieldDescription, FieldError, FieldLabel } from "../../components/ui/field";

type ControlBinding<T extends FieldValues, N extends FieldPath<T>> = ControllerRenderProps<T, N> & {
  id: string;
  "aria-invalid": boolean;
  "aria-required": boolean;
  "aria-describedby": string | undefined;
};

type FormFieldProps<T extends FieldValues, N extends FieldPath<T>> = UseControllerProps<T, N> & {
  label: ReactNode;
  description?: ReactNode;
  required?: boolean;
  children: (binding: ControlBinding<T, N>) => ReactNode;
};

/** One controller per logical control; validation rules belong to the caller's server-backed schema. */
export function FormField<T extends FieldValues, N extends FieldPath<T>>({
  label, description, required, children, ...controller
}: FormFieldProps<T, N>) {
  const id = useId();
  const { field, fieldState } = useController(controller);
  const error = fieldState.error?.message;
  const describedBy = [description ? `${id}-description` : null, error ? `${id}-error` : null].filter(Boolean).join(" ") || undefined;
  return (
    <Field name={field.name} invalid={fieldState.invalid} disabled={field.disabled} data-form-field={field.name}>
      <FieldLabel htmlFor={id}>{label}{required && <span aria-hidden="true"> *</span>}</FieldLabel>
      {children({ ...field, id, "aria-required": !!required, "aria-invalid": fieldState.invalid, "aria-describedby": describedBy })}
      {description && <FieldDescription id={`${id}-description`}>{description}</FieldDescription>}
      {error && <FieldError id={`${id}-error`} match role="alert">{error}</FieldError>}
    </Field>
  );
}
