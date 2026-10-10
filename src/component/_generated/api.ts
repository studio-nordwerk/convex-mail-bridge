/* eslint-disable */
/**
 * Generated `api` utility.
 *
 * THIS CODE IS AUTOMATICALLY GENERATED.
 *
 * To regenerate, run `npx convex dev`.
 * @module
 */

import type * as devices from "../devices.js";
import type * as entries from "../entries.js";
import type * as folders from "../folders.js";
import type * as lib_access from "../lib/access.js";
import type * as lib_flags from "../lib/flags.js";
import type * as lib_ledger from "../lib/ledger.js";
import type * as lib_place from "../lib/place.js";
import type * as lib_setupLinks from "../lib/setupLinks.js";
import type * as mailbox from "../mailbox.js";
import type * as maintenance from "../maintenance.js";
import type * as sessions from "../sessions.js";
import type * as setup from "../setup.js";

import type {
  ApiFromModules,
  FilterApi,
  FunctionReference,
} from "convex/server";
import { anyApi, componentsGeneric } from "convex/server";

const fullApi: ApiFromModules<{
  devices: typeof devices;
  entries: typeof entries;
  folders: typeof folders;
  "lib/access": typeof lib_access;
  "lib/flags": typeof lib_flags;
  "lib/ledger": typeof lib_ledger;
  "lib/place": typeof lib_place;
  "lib/setupLinks": typeof lib_setupLinks;
  mailbox: typeof mailbox;
  maintenance: typeof maintenance;
  sessions: typeof sessions;
  setup: typeof setup;
}> = anyApi as any;

/**
 * A utility for referencing Convex functions in your app's public API.
 *
 * Usage:
 * ```js
 * const myFunctionReference = api.myModule.myFunction;
 * ```
 */
export const api: FilterApi<
  typeof fullApi,
  FunctionReference<any, "public">
> = anyApi as any;

/**
 * A utility for referencing Convex functions in your app's internal API.
 *
 * Usage:
 * ```js
 * const myFunctionReference = internal.myModule.myFunction;
 * ```
 */
export const internal: FilterApi<
  typeof fullApi,
  FunctionReference<any, "internal">
> = anyApi as any;

export const components = componentsGeneric() as unknown as {};
