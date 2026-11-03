import { useState } from "react";
import { cn, initials } from "@/lib/utils";

// Avatar backgrounds are picked from the name, so a person keeps their color.
const tints = [
  "bg-[#e1e3ff] text-[#3a2fb0] dark:bg-[#26235a] dark:text-[#c8ccff]",
  "bg-[#dcf5e7] text-[#13643a] dark:bg-[#123222] dark:text-[#9be8bb]",
  "bg-[#fde7d4] text-[#8a3d0b] dark:bg-[#3a2212] dark:text-[#f9c39b]",
  "bg-[#fbe0ea] text-[#8f1f4c] dark:bg-[#3b1525] dark:text-[#f6b2cc]",
  "bg-[#d9eefc] text-[#0c537f] dark:bg-[#11293b] dark:text-[#a3d4f6]",
  "bg-[#ece2fd] text-[#5b259c] dark:bg-[#2c1a45] dark:text-[#d4bcfa]",
];

function tint(seed: string): string {
  let h = 0;
  for (const c of seed) {
    h = (h * 31 + c.charCodeAt(0)) >>> 0;
  }
  return tints[h % tints.length] ?? tints[0] ?? "";
}

export function Avatar({
  name,
  src,
  seed,
  className,
}: {
  name: string;
  src?: string | undefined;
  seed?: string;
  className?: string;
}) {
  const [failed, setFailed] = useState<string>();
  return (
    <span
      aria-hidden="true"
      className={cn(
        "inline-flex size-8 shrink-0 select-none items-center justify-center rounded-full font-semibold text-xs",
        tint(seed ?? name),
        className,
      )}
    >
      {src && failed !== src ? (
        <img
          src={src}
          alt=""
          className="size-full rounded-full object-cover"
          onError={() => setFailed(src)}
        />
      ) : (
        initials(name)
      )}
    </span>
  );
}
