import { defineMessages, type IntlShape } from "react-intl";
import type { Schema } from "./api";

export const fieldMessages = defineMessages({
  title: { id: "profileFields.title", defaultMessage: "Profile fields" },
  description: {
    id: "profileFields.description",
    defaultMessage: "Choose what people share on their profiles.",
  },
  library: { id: "profileFields.library", defaultMessage: "Field library" },
  custom: { id: "profileFields.custom", defaultMessage: "Custom fields" },
  add: { id: "profileFields.add", defaultMessage: "Create custom field" },
  enable: { id: "profileFields.enable", defaultMessage: "Enable" },
  disable: { id: "profileFields.disable", defaultMessage: "Disable" },
  archive: { id: "profileFields.archive", defaultMessage: "Archive" },
  restore: { id: "profileFields.restore", defaultMessage: "Restore" },
  settings: { id: "profileFields.settings", defaultMessage: "Settings" },
  advanced: { id: "profileFields.advanced", defaultMessage: "Advanced settings" },
  name: { id: "profileFields.name", defaultMessage: "Field name" },
  type: { id: "profileFields.type", defaultMessage: "Field type" },
  visibility: { id: "profileFields.visibility", defaultMessage: "Visibility" },
  options: { id: "profileFields.options", defaultMessage: "Choices (one per line)" },
  section: { id: "profileFields.section", defaultMessage: "Section" },
  editor: { id: "profileFields.editor", defaultMessage: "Who can edit" },
  handleKind: { id: "profileFields.handleKind", defaultMessage: "Handle kind" },
  required: { id: "profileFields.required", defaultMessage: "Required at onboarding" },
  hideable: { id: "profileFields.hideable", defaultMessage: "People can hide their value" },
  sensitive: { id: "profileFields.sensitive", defaultMessage: "Sensitive (encrypted and audited)" },
  sensitiveHint: {
    id: "profileFields.sensitiveHint",
    defaultMessage: "Sensitive values require permission to read personal data.",
  },
  order: { id: "profileFields.order", defaultMessage: "Order within section" },
  next: { id: "profileFields.next", defaultMessage: "Next" },
  back: { id: "profileFields.back", defaultMessage: "Back" },
  save: { id: "common.save", defaultMessage: "Save" },
  cancel: { id: "common.cancel", defaultMessage: "Cancel" },
  create: { id: "profileFields.create", defaultMessage: "Create field" },
  stepName: { id: "profileFields.stepName", defaultMessage: "1. Name" },
  stepType: { id: "profileFields.stepType", defaultMessage: "2. Type" },
  stepVisibility: { id: "profileFields.stepVisibility", defaultMessage: "3. Visibility" },
  everyone: { id: "profileFields.visibility.everyone", defaultMessage: "Everyone" },
  team: { id: "profileFields.visibility.team", defaultMessage: "Team" },
  admins: { id: "profileFields.visibility.admins", defaultMessage: "Admins only" },
  self: { id: "profileFields.editor.self", defaultMessage: "Person and admins" },
  adminsEditor: { id: "profileFields.editor.admins", defaultMessage: "Admins only" },
  contacts: { id: "profileFields.section.contacts", defaultMessage: "Contacts" },
  personal: { id: "profileFields.section.personal", defaultMessage: "Personal" },
  hr: { id: "profileFields.section.hr", defaultMessage: "HR" },
  customSection: { id: "profileFields.section.custom", defaultMessage: "Custom" },
  text: { id: "profileFields.type.text", defaultMessage: "Text" },
  long_text: { id: "profileFields.type.longText", defaultMessage: "Long text" },
  date: { id: "profileFields.type.date", defaultMessage: "Date" },
  select: { id: "profileFields.type.select", defaultMessage: "Select" },
  phoneType: { id: "profileFields.type.phone", defaultMessage: "Phone" },
  url: { id: "profileFields.type.url", defaultMessage: "URL" },
  handle: { id: "profileFields.type.handle", defaultMessage: "Handle" },
  telegram: { id: "profileFields.library.telegram", defaultMessage: "Telegram" },
  github: { id: "profileFields.library.github", defaultMessage: "GitHub" },
  phone: { id: "profileFields.library.phone", defaultMessage: "Phone" },
  birthday: { id: "profileFields.library.birthday", defaultMessage: "Birthday" },
  tshirt_size: { id: "profileFields.library.tshirtSize", defaultMessage: "T-shirt size" },
  pronouns: { id: "profileFields.library.pronouns", defaultMessage: "Pronouns" },
  location: { id: "profileFields.library.location", defaultMessage: "Location" },
  start_date: { id: "profileFields.library.startDate", defaultMessage: "Start date" },
  emergency_contact: {
    id: "profileFields.library.emergencyContact",
    defaultMessage: "Emergency contact",
  },
  gitlab: { id: "profileFields.handle.gitlab", defaultMessage: "GitLab" },
  x: { id: "profileFields.handle.x", defaultMessage: "X" },
  mastodon: { id: "profileFields.handle.mastodon", defaultMessage: "Mastodon" },
  myProfile: { id: "profile.myProfile", defaultMessage: "My profile" },
  profile: { id: "profile.title", defaultMessage: "Profile" },
  empty: { id: "profile.empty", defaultMessage: "No profile fields to show yet." },
  edit: { id: "profile.edit", defaultMessage: "Edit" },
  clear: { id: "profile.clear", defaultMessage: "Clear value" },
  hidden: { id: "profile.hidden", defaultMessage: "Hidden from others" },
  hide: { id: "profile.hide", defaultMessage: "Hide from others" },
  yearHint: {
    id: "profile.yearHint",
    defaultMessage: "Only you and admins with sensitive access see the birth year.",
  },
  invalid: { id: "profileFields.invalid", defaultMessage: "Check the field values and try again." },
  conflict: {
    id: "profileFields.conflict",
    defaultMessage: "This change conflicts with an existing field or a choice that is in use.",
  },
  loading: { id: "profileFields.loading", defaultMessage: "Loading profile fields\u2026" },
  retry: { id: "common.retry", defaultMessage: "Try again" },
  forbidden: {
    id: "profileFields.forbidden",
    defaultMessage: "You need permission to manage people to configure profile fields.",
  },
  notFound: { id: "profile.notFound", defaultMessage: "This profile is unavailable." },
  saved: { id: "profile.saved", defaultMessage: "Saved." },
});

export type ProfileField = Schema<"ProfileField">;
export type FieldType = Schema<"ProfileFieldType">;
export const fieldTypes: FieldType[] = [
  "text",
  "long_text",
  "date",
  "select",
  "phone",
  "url",
  "handle",
];
export const sections = ["contacts", "personal", "hr", "custom"] as const;
export const visibilities = ["everyone", "team", "admins"] as const;
const libraryMessages = {
  telegram: fieldMessages.telegram,
  github: fieldMessages.github,
  phone: fieldMessages.phone,
  birthday: fieldMessages.birthday,
  tshirt_size: fieldMessages.tshirt_size,
  pronouns: fieldMessages.pronouns,
  location: fieldMessages.location,
  start_date: fieldMessages.start_date,
  emergency_contact: fieldMessages.emergency_contact,
};
export function fieldLabel(
  intl: IntlShape,
  field: { key: string; label: string; library?: boolean },
): string {
  const message = libraryMessages[field.key as keyof typeof libraryMessages];
  return field.library !== false && message ? intl.formatMessage(message) : field.label;
}
export const typeMessages = {
  text: fieldMessages.text,
  long_text: fieldMessages.long_text,
  date: fieldMessages.date,
  select: fieldMessages.select,
  phone: fieldMessages.phoneType,
  url: fieldMessages.url,
  handle: fieldMessages.handle,
};
export const sectionMessages = {
  contacts: fieldMessages.contacts,
  personal: fieldMessages.personal,
  hr: fieldMessages.hr,
  custom: fieldMessages.customSection,
};
export const handleMessages = {
  telegram: fieldMessages.telegram,
  github: fieldMessages.github,
  gitlab: fieldMessages.gitlab,
  x: fieldMessages.x,
  mastodon: fieldMessages.mastodon,
};
export const fieldsKey = ["profile-fields"] as const;
export const libraryKey = ["profile-field-library"] as const;
export const profileKey = (id: string) => ["person-profile", id] as const;

/** Format calendar dates without shifting them into the viewer's timezone. */
export function displayValue(intl: IntlShape, entry: Schema<"ProfileFieldValue">): string {
  const value = entry.value ?? "";
  if (!value) return "";
  if (entry.field.type === "select")
    return entry.field.options.find((o) => o.key === value)?.label ?? value;
  if (entry.field.type !== "date") return value;
  const date = new Date(`${entry.yearHidden ? `2000${value.slice(1)}` : value}T00:00:00Z`);
  if (Number.isNaN(date.getTime())) return value;
  return intl.formatDate(date, {
    day: "numeric",
    month: "long",
    ...(entry.yearHidden ? {} : { year: "numeric" }),
    timeZone: "UTC",
  });
}
