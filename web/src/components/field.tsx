import { Eye, EyeOff } from "lucide-react";
import { type ComponentProps, type ReactNode, useId, useState } from "react";
import { defineMessages, useIntl } from "react-intl";

import { cn } from "@/lib/utils";

import { Input } from "./ui/input";
import { Label } from "./ui/label";

const messages = defineMessages({
  show: { id: "field.password.show", defaultMessage: "Show password" },
  hide: { id: "field.password.hide", defaultMessage: "Hide password" },
});

interface FieldProps extends Omit<ComponentProps<"input">, "id"> {
  label: ReactNode;
  /** Help below the input; replaced by the error when there is one. */
  hint?: ReactNode;
  error?: string | undefined;
  /** Something next to the label, e.g. a "Forgot password?" link. */
  aside?: ReactNode;
  id?: string;
}

/** A labeled input with hint and error text wired up for assistive technology. */
export function Field({ label, hint, error, aside, id, className, type, ...props }: FieldProps) {
  const autoId = useId();
  const inputId = id ?? autoId;
  const describedBy = `${inputId}-description`;
  const [revealed, setRevealed] = useState(false);
  const intl = useIntl();
  const isPassword = type === "password";

  return (
    <div className={cn("flex flex-col gap-2", className)}>
      <div className="flex items-center justify-between gap-2">
        <Label htmlFor={inputId}>{label}</Label>
        {aside}
      </div>
      <div className="relative">
        <Input
          id={inputId}
          type={isPassword && revealed ? "text" : type}
          aria-invalid={error ? true : undefined}
          aria-describedby={error || hint ? describedBy : undefined}
          className={cn(isPassword && "pr-10")}
          {...props}
        />
        {isPassword && (
          <button
            type="button"
            className="absolute inset-y-0 right-0 flex w-10 cursor-pointer items-center justify-center rounded-r-md text-muted-foreground outline-none transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
            aria-label={intl.formatMessage(revealed ? messages.hide : messages.show)}
            aria-pressed={revealed}
            onClick={() => setRevealed((v) => !v)}
          >
            {revealed ? (
              <EyeOff className="size-4" aria-hidden="true" />
            ) : (
              <Eye className="size-4" aria-hidden="true" />
            )}
          </button>
        )}
      </div>
      {(error || hint) && (
        <p
          id={describedBy}
          className={cn(
            "text-[13px] leading-5",
            error ? "text-destructive-subtle-foreground" : "text-muted-foreground",
          )}
        >
          {error ?? hint}
        </p>
      )}
    </div>
  );
}
