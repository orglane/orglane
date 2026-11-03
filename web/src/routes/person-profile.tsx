import { useIntl } from "react-intl";
import { Page } from "@/components/page";
import { ProfileFieldsCard } from "@/components/profile-fields-card";
import { fieldMessages } from "@/lib/profile-fields";
import { useMe } from "@/lib/session";

/** Profile fields can also be embedded into the full directory person page. */
export function PersonProfilePage({ personId }: { personId: string }) {
  const intl = useIntl();
  const me = useMe();
  return (
    <Page
      title={intl.formatMessage(
        personId === me.person.id ? fieldMessages.myProfile : fieldMessages.profile,
      )}
    >
      <ProfileFieldsCard personId={personId} />
    </Page>
  );
}
