import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { FormattedMessage, useIntl } from "react-intl";
import { AuthLayout } from "@/components/auth-layout";
import { OnboardingSteps } from "@/components/onboarding-checklist";
import { ProfileFieldsCard } from "@/components/profile-fields-card";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { call, meKey } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { useDocumentTitle } from "@/lib/document-title";
import { errorMessage } from "@/lib/errors";
import { onboardingMessages as m, onboardingKey, onboardingQuery } from "@/lib/onboarding";
import { useMe } from "@/lib/session";
import { safeRedirect } from "@/lib/utils";

export function WelcomePage({ redirect }: { redirect?: string | undefined }) {
  const intl = useIntl();
  const api = useApi();
  const me = useMe();
  const client = useQueryClient();
  const navigate = useNavigate();
  const progress = useQuery(onboardingQuery(api));
  useDocumentTitle(intl.formatMessage(m.welcome));
  const finish = useMutation({
    mutationFn: () => call(api.client.POST("/api/v1/me/onboarding/finish")),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: meKey });
      await client.invalidateQueries({ queryKey: onboardingKey });
      await navigate({ href: safeRedirect(redirect), replace: true });
    },
    onError: () => {
      void client.invalidateQueries({ queryKey: onboardingKey });
    },
  });
  const requiredIncomplete = progress.data?.steps.some((s) => s.required && !s.complete) ?? true;
  return (
    <AuthLayout>
      <div className="space-y-6 rounded-xl border bg-surface p-6 shadow-lg sm:p-8">
        <h1 className="font-semibold text-2xl">
          <FormattedMessage {...m.welcome} />
        </h1>
        <p className="text-muted-foreground text-sm">
          <FormattedMessage {...m.intro} />
        </p>
        {progress.isPending && (
          <p role="status">
            <FormattedMessage {...m.loading} />
          </p>
        )}
        {progress.error && (
          <>
            <Alert>{errorMessage(intl, progress.error)}</Alert>
            <Button onClick={() => void progress.refetch()}>
              <FormattedMessage {...m.retry} />
            </Button>
          </>
        )}
        {progress.data && (
          <>
            <OnboardingSteps steps={progress.data.steps} />
            {requiredIncomplete && <ProfileFieldsCard personId={me.person.id} requiredOnly />}
            {finish.error && <Alert>{errorMessage(intl, finish.error, { 409: m.changed })}</Alert>}
            <p className="text-muted-foreground text-sm">
              <FormattedMessage {...m.requiredHint} />
            </p>
            <Button
              className="w-full"
              disabled={requiredIncomplete}
              loading={finish.isPending}
              onClick={() => finish.mutate()}
            >
              <FormattedMessage
                {...(progress.data.steps.some((s) => !s.required && !s.complete)
                  ? m.continue
                  : m.done)}
              />
            </Button>
          </>
        )}
      </div>
    </AuthLayout>
  );
}
