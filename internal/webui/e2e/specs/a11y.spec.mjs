// The two criteria that exist ONLY at the engine: what Tab actually focuses, and what the
// accessibility tree says each control is called.
//
// Neither is observable from the document. A KeyboardEvent constructed inside the page is
// untrusted and moves focus nowhere, so tab order cannot be read by any expression the
// page evaluates; and an accessible name is the engine's own answer over labels, ARIA,
// native semantics and content, not a property of markup anything can reconstruct by
// inspection. This is why these were driven over the DevTools protocol by hand, and it is
// exactly the vocabulary the runner already speaks.
import { test, expect } from "@playwright/test";
import { open, waitRendered, axTree } from "./conventions.mjs";
import { pageURL } from "./theme.mjs";
import { tabThroughEveryControl, gradeTabOrderAndFocusRing, gradeAccessibleNames } from "./a11y.mjs";

