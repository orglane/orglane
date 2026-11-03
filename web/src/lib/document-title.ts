import { useEffect } from "react";

import { productName } from "./brand";

/** Sets the tab title: "Inbox · Orglane". */
export function useDocumentTitle(title: string | undefined): void {
  useEffect(() => {
    document.title = title ? `${title} · ${productName}` : productName;
  }, [title]);
}
