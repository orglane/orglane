import { useQuery } from "@tanstack/react-query";
import { FormattedMessage, useIntl } from "react-intl";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import type { Schema } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { errorMessage } from "@/lib/errors";
import { onboardingMessages as m, onboardingQuery } from "@/lib/onboarding";

const builtinMessages = {
  profile: [m.profileTitle, m.profileDescription],
  "profile-admin": [m.adminStepTitle, m.adminStepDescription],
  avatar: [m.avatarTitle, m.avatarDescription],
  passkey: [m.passkeyTitle, m.passkeyDescription],
} as const;

export function OnboardingSteps({ steps }: { steps: Schema<"OnboardingStep">[] }) {
  const intl = useIntl();
  return (
    <ul className="divide-y">
      {steps.map((step) => {
        const translated = builtinMessages[step.id as keyof typeof builtinMessages];
        const title = translated ? intl.formatMessage(translated[0]) : step.title;
        return (
          <li key={step.id} className="py-4" data-testid={`onboarding-${step.id}`}>
            <div className="flex flex-wrap items-center justify-between gap-3">
              <h3 className="font-medium">{title}</h3>
              <span
                role="status"
                aria-live="polite"
                aria-atomic="true"
                aria-label={title}
                className="text-muted-foreground text-sm"
              >
                <FormattedMessage {...(step.complete ? m.complete : m.incomplete)} />
                {" · "}
                <FormattedMessage {...(step.required ? m.required : m.optional)} />
              </span>
            </div>
            <p className="mt-1 text-muted-foreground text-sm">
              {translated ? intl.formatMessage(translated[1]) : step.description}
            </p>
            {!step.complete && step.actionUrl && (
              <a
                className="mt-2 inline-block text-sm underline"
                href={step.actionUrl}
                aria-label={`${intl.formatMessage(m.action)}: ${title}`}
              >
                <FormattedMessage {...m.action} />
              </a>
            )}
          </li>
        );
      })}
    </ul>
  );
}

/** Admin progress is fetched only with people.manage. No profile values enter this component. */
export function OnboardingChecklist({ personId }: { personId?: string }) {
  const api = useApi();
  const intl = useIntl();
  const progress = useQuery(onboardingQuery(api, personId));
  return (
    <Card className="p-5 sm:p-6">
      <h2 className="font-semibold text-lg">
        <FormattedMessage {...(personId ? m.adminTitle : m.title)} />
      </h2>
      {personId && (
        <p className="mt-2 text-muted-foreground text-sm">
          <FormattedMessage {...m.adminHint} />
        </p>
      )}
      {progress.isPending && (
        <p role="status">
          <FormattedMessage {...m.loading} />
        </p>
      )}
      {progress.error && (
        <div className="mt-3 space-y-3">
          <Alert>{errorMessage(intl, progress.error)}</Alert>
          <Button onClick={() => void progress.refetch()}>
            <FormattedMessage {...m.retry} />
          </Button>
        </div>
      )}
      {progress.data && <OnboardingSteps steps={progress.data.steps} />}
      {progress.data?.welcomePending && (
        <a className="mt-3 inline-block underline" href="/welcome">
          <FormattedMessage {...m.resume} />
        </a>
      )}
    </Card>
  );
}
