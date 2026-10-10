/* eslint-disable */
/**
 * Generated `ComponentApi` utility.
 *
 * THIS CODE IS AUTOMATICALLY GENERATED.
 *
 * To regenerate, run `npx convex dev`.
 * @module
 */

import type { FunctionReference } from "convex/server";

/**
 * A utility for referencing a Convex component's exposed API.
 *
 * Useful when expecting a parameter like `components.myComponent`.
 * Usage:
 * ```ts
 * async function myFunction(ctx: QueryCtx, component: ComponentApi) {
 *   return ctx.runQuery(component.someFile.someQuery, { ...args });
 * }
 * ```
 */
export type ComponentApi<Name extends string | undefined = string | undefined> =
  {
    devices: {
      create: FunctionReference<
        "mutation",
        "internal",
        {
          label: string;
          mailboxKey: string;
          ownerKey: string;
          secretHash: string;
          userKey?: string;
        },
        | { createdAt: number; id: string; kind: "created" }
        | { kind: "limit" }
        | { kind: "disabled" }
        | { kind: "owner" },
        Name
      >;
      holder: FunctionReference<
        "query",
        "internal",
        { mailboxKey: string; secretHash: string },
        null | { userKey: string | null },
        Name
      >;
      list: FunctionReference<
        "query",
        "internal",
        { mailboxKey: string; userKey?: string },
        Array<{
          createdAt: number;
          id: string;
          label: string;
          lastUsedAt?: number;
        }>,
        Name
      >;
      revoke: FunctionReference<
        "mutation",
        "internal",
        { id: string; mailboxKey: string; userKey?: string },
        boolean,
        Name
      >;
      revokeAll: FunctionReference<
        "mutation",
        "internal",
        { mailboxKey: string },
        { devices: number; sessions: number; setupLinks: number },
        Name
      >;
      revokeUser: FunctionReference<
        "mutation",
        "internal",
        { mailboxKey: string; userKey: string },
        { devices: number; sessions: number; setupLinks: number },
        Name
      >;
    };
    entries: {
      add: FunctionReference<
        "mutation",
        "internal",
        {
          flags?: Array<string>;
          folder: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
          groupKey?: string;
          internalDate: number;
          kind?: "in" | "out";
          mailboxKey: string;
          messageKey: string;
          seen: boolean;
          size: number;
        },
        null | { created: boolean; moved: boolean; uid: number },
        Name
      >;
      byMessage: FunctionReference<
        "query",
        "internal",
        { messageKey: string },
        null | {
          flags: Array<string>;
          folder: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
          groupKey?: string;
          internalDate: number;
          kind?: "in" | "out";
          live: boolean;
          mailboxKey: string;
          messageKey: string;
          modseq: number;
          removedAt?: number;
          removedBy?: "client" | "purge" | "move";
          removedModseq?: number;
          size: number;
          uid: number;
        },
        Name
      >;
      byUid: FunctionReference<
        "query",
        "internal",
        {
          folder: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
          mailboxKey: string;
          uid: number;
        },
        null | {
          flags: Array<string>;
          internalDate: number;
          messageKey: string;
          modseq: number;
          size: number;
          uid: number;
        },
        Name
      >;
      changes: FunctionReference<
        "query",
        "internal",
        {
          folder: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
          limit: number;
          mailboxKey: string;
          sinceModseq: number;
        },
        {
          isDone: boolean;
          resync: boolean;
          rows: Array<{
            flags: Array<string>;
            internalDate: number;
            live: boolean;
            messageKey: string;
            modseq: number;
            removedAt?: number;
            removedBy?: "client" | "purge" | "move";
            removedModseq?: number;
            size: number;
            uid: number;
          }>;
        },
        Name
      >;
      hide: FunctionReference<
        "mutation",
        "internal",
        {
          folder: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
          mailboxKey: string;
          uids: Array<number>;
        },
        Array<{ messageKey: string; uid: number }>,
        Name
      >;
      list: FunctionReference<
        "query",
        "internal",
        {
          afterUid?: number;
          folder: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
          limit: number;
          mailboxKey: string;
        },
        {
          isDone: boolean;
          rows: Array<{
            flags: Array<string>;
            internalDate: number;
            messageKey: string;
            modseq: number;
            size: number;
            uid: number;
          }>;
        },
        Name
      >;
      moveGroup: FunctionReference<
        "mutation",
        "internal",
        {
          groupKey: string;
          limit: number;
          mailboxKey: string;
          targets: {
            in?: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
            out?: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
          };
        },
        {
          isDone: boolean;
          moved: Array<{
            from: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
            fromLive: boolean;
            fromUid: number;
            messageKey: string;
            to: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
            uid: number;
          }>;
        },
        Name
      >;
      remove: FunctionReference<
        "mutation",
        "internal",
        { messageKey: string },
        boolean,
        Name
      >;
      resolve: FunctionReference<
        "query",
        "internal",
        {
          folder: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
          mailboxKey: string;
          uid: number;
        },
        null | {
          flags: Array<string>;
          folder: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
          internalDate: number;
          messageKey: string;
          modseq: number;
          moved: boolean;
          size: number;
          uid: number;
        },
        Name
      >;
      setFlags: FunctionReference<
        "mutation",
        "internal",
        {
          add: Array<string>;
          folder: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
          mailboxKey: string;
          remove: Array<string>;
          uids: Array<number>;
        },
        Array<{
          changed: boolean;
          flags: Array<string>;
          messageKey: string;
          modseq: number;
          seenChanged: boolean;
          uid: number;
        }>,
        Name
      >;
      setSeenMany: FunctionReference<
        "mutation",
        "internal",
        {
          changes: Array<{ messageKey: string; seen: boolean }>;
          mailboxKey: string;
        },
        number,
        Name
      >;
      tagMany: FunctionReference<
        "mutation",
        "internal",
        {
          mailboxKey: string;
          rows: Array<{
            groupKey: string;
            kind: "in" | "out";
            messageKey: string;
          }>;
        },
        number,
        Name
      >;
    };
    folders: {
      list: FunctionReference<
        "query",
        "internal",
        { mailboxKey: string },
        Array<{
          exists: number;
          folder: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
          highestModseq: number;
          uidNext: number;
          uidValidity: number;
          unseen: number;
        }>,
        Name
      >;
    };
    mailbox: {
      addFolders: FunctionReference<
        "mutation",
        "internal",
        {
          folders: Array<"INBOX" | "Sent" | "Archive" | "Trash" | "Later">;
          mailboxKey: string;
        },
        null | Array<"INBOX" | "Sent" | "Archive" | "Trash" | "Later">,
        Name
      >;
      enable: FunctionReference<
        "mutation",
        "internal",
        { mailboxKey: string; ownerKey: string },
        { created: boolean; enabledAt: number },
        Name
      >;
      get: FunctionReference<
        "query",
        "internal",
        { mailboxKey: string },
        null | {
          backfillCursor?: string;
          backfillFolder?: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
          backfillRun: number;
          backfillStatus: "running" | "done";
          backfillUpdatedAt: number;
          enabledAt: number;
          ownerKey: string;
          repairCursor?: string;
          seq: number;
        },
        Name
      >;
      list: FunctionReference<
        "query",
        "internal",
        { afterKey?: string; limit: number },
        {
          isDone: boolean;
          rows: Array<{
            backfillStatus: "running" | "done";
            backfillUpdatedAt: number;
            mailboxKey: string;
          }>;
        },
        Name
      >;
      setBackfill: FunctionReference<
        "mutation",
        "internal",
        {
          cursor?: string;
          folder?: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
          mailboxKey: string;
          run?: number;
          status: "running" | "done";
        },
        boolean,
        Name
      >;
      setRepairCursor: FunctionReference<
        "mutation",
        "internal",
        { cursor?: string; mailboxKey: string },
        boolean,
        Name
      >;
    };
    maintenance: {
      cleanup: FunctionReference<
        "mutation",
        "internal",
        { before: number; limit: number },
        { deleted: number; isDone: boolean },
        Name
      >;
      scan: FunctionReference<
        "query",
        "internal",
        {
          afterUid?: number;
          folder: "INBOX" | "Sent" | "Archive" | "Trash" | "Later";
          limit: number;
          live: boolean;
          mailboxKey: string;
        },
        {
          isDone: boolean;
          rows: Array<{
            flags: Array<string>;
            messageKey: string;
            removedBy?: "client" | "purge" | "move";
            tagged: boolean;
            uid: number;
          }>;
        },
        Name
      >;
    };
    sessions: {
      check: FunctionReference<
        "query",
        "internal",
        { now: number; tokenHash: string },
        null | {
          devicePasswordId: string;
          expiresAt: number;
          mailboxKey: string;
          ownerKey: string;
          userKey: string;
        },
        Name
      >;
      cleanup: FunctionReference<
        "mutation",
        "internal",
        { limit: number; now: number },
        {
          attempts: number;
          devices: number;
          isDone: boolean;
          sessions: number;
          setupLinks: number;
        },
        Name
      >;
      login: FunctionReference<
        "mutation",
        "internal",
        {
          ipKey: string;
          issue?: boolean;
          mailboxKey: string | null;
          ownerKey: string | null;
          pairKey: string;
          secretHash: string;
          tokenHash: string;
        },
        | { expiresAt: number; kind: "ok" }
        | { kind: "auth" }
        | { kind: "preparing" }
        | { kind: "throttled"; retryAfterSeconds: number },
        Name
      >;
      seqMany: FunctionReference<
        "query",
        "internal",
        { now: number; tokenHashes: Array<string> },
        Array<null | {
          mailboxKey: string;
          ownerKey: string;
          seq: number;
          userKey: string;
        }>,
        Name
      >;
    };
    setup: {
      claim: FunctionReference<
        "mutation",
        "internal",
        { ownerKey: string; ticketHash: string; tokenHash: string },
        | { expiresAt: number; id: string; kind: "claimed" }
        | { kind: "gone" }
        | { kind: "limit" },
        Name
      >;
      create: FunctionReference<
        "mutation",
        "internal",
        {
          label: string;
          mailboxKey: string;
          ownerKey: string;
          tokenHash: string;
          userKey?: string;
        },
        | { expiresAt: number; id: string; kind: "created" }
        | { kind: "disabled" }
        | { kind: "owner" }
        | { kind: "devices" }
        | { kind: "links" },
        Name
      >;
      discard: FunctionReference<
        "mutation",
        "internal",
        {
          id: string;
          mailboxKey: string;
          onlyOpen?: boolean;
          userKey?: string;
        },
        boolean,
        Name
      >;
      get: FunctionReference<
        "query",
        "internal",
        { id: string },
        null | {
          deviceId: string | null;
          expiresAt: number;
          id: string;
          label: string;
          mailboxKey: string;
          ownerKey: string;
          state: "open" | "claimed" | "downloaded";
          userKey: string | null;
        },
        Name
      >;
      peek: FunctionReference<
        "query",
        "internal",
        { now: number; tokenHash: string },
        null | {
          expiresAt: number;
          id: string;
          label: string;
          mailboxKey: string;
          ownerKey: string;
          userKey: string | null;
        },
        Name
      >;
      peekTicket: FunctionReference<
        "query",
        "internal",
        { now: number; ticketHash: string },
        null | {
          id: string;
          label: string;
          mailboxKey: string;
          ownerKey: string;
          userKey: string | null;
        },
        Name
      >;
      redeem: FunctionReference<
        "mutation",
        "internal",
        { ownerKey: string; secretHash: string; ticketHash: string },
        | { deviceId: string; id: string; kind: "created"; label: string }
        | { kind: "gone" }
        | { kind: "limit" },
        Name
      >;
    };
  };
