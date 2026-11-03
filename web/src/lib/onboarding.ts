import { queryOptions } from "@tanstack/react-query";
import { defineMessages } from "react-intl";
import { type Api, call } from "./api";

export const onboardingKey = ["onboarding"] as const;
export const onboardingQuery = (api: Api, personId?: string) =>
  queryOptions({
    queryKey: [...onboardingKey, personId ?? "self"],
    queryFn: () =>
      personId
        ? call(
            api.client.GET("/api/v1/people/{personId}/onboarding", {
              params: { path: { personId } },
            }),
          )
        : call(api.client.GET("/api/v1/me/onboarding")),
    staleTime: 0,
    refetchOnMount: "always",
    retry: false,
  });

export const onboardingMessages = defineMessages({
  title: { id: "onboarding.title", defaultMessage: "Getting started" },
  adminTitle: { id: "onboarding.adminTitle", defaultMessage: "Onboarding progress" },
  adminHint: {
    id: "onboarding.adminHint",
    defaultMessage:
      "Progress covers profile information you are allowed to see. Private fields are excluded.",
  },
  welcome: { id: "onboarding.welcome", defaultMessage: "Welcome to your workspace" },
  intro: {
    id: "onboarding.intro",
    defaultMessage:
      "A few quick steps to help you settle in. Complete your required profile information, then choose what to set up now.",
  },
  required: { id: "onboarding.required", defaultMessage: "Required" },
  optional: { id: "onboarding.optional", defaultMessage: "Optional" },
  complete: { id: "onboarding.complete", defaultMessage: "Complete" },
  incomplete: { id: "onboarding.incomplete", defaultMessage: "To do" },
  action: { id: "onboarding.action", defaultMessage: "Open step" },
  continue: { id: "onboarding.continue", defaultMessage: "Skip optional steps and continue" },
  done: { id: "onboarding.done", defaultMessage: "Go to Home" },
  resume: { id: "onboarding.resume", defaultMessage: "Continue welcome" },
  loading: { id: "onboarding.loading", defaultMessage: "Loading your checklist…" },
  retry: { id: "onboarding.retry", defaultMessage: "Try again" },
  requiredHint: {
    id: "onboarding.requiredHint",
    defaultMessage:
      "Complete the required profile step to continue. Optional steps can wait; they stay on your Home checklist.",
  },
  changed: {
    id: "onboarding.changed",
    defaultMessage: "Your required steps have changed. Review your checklist and try again.",
  },
  profileTitle: { id: "onboarding.profile.title", defaultMessage: "Complete your profile" },
  profileDescription: {
    id: "onboarding.profile.description",
    defaultMessage: "Fill in the required profile fields you can edit.",
  },
  adminStepTitle: { id: "onboarding.adminStep.title", defaultMessage: "Administrator follow-up" },
  adminStepDescription: {
    id: "onboarding.adminStep.description",
    defaultMessage:
      "Some required profile information needs an administrator. You can continue welcome.",
  },
  avatarTitle: { id: "onboarding.avatar.title", defaultMessage: "Upload a photo" },
  avatarDescription: {
    id: "onboarding.avatar.description",
    defaultMessage: "Help your teammates recognize you.",
  },
  passkeyTitle: { id: "onboarding.passkey.title", defaultMessage: "Add a passkey" },
  passkeyDescription: {
    id: "onboarding.passkey.description",
    defaultMessage: "Sign in with your fingerprint, face or screen lock. You can do this later.",
  },
});
