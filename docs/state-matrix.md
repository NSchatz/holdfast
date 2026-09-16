# The dashboard's state matrix

What every interactive component on the dashboard renders in each of the seven states
`interface-craft` C7 names - default, hover, focus-visible, active, disabled, loading and
error - and the densities this surface builds.

This file is READ BY A GRADER. `internal/webui/e2e/specs/statematrix.spec.mjs` derives the
component inventory from the served document at the engine, looks every derived component up
here, and then enters every state this file calls `proved` for real and requires it to render
differently from that component's default. A component the engine derives and this file does
not name fails the run, so a control cannot enter the surface without entering the matrix;
and a state this file calls `proved` that renders identically to the default fails it too, so
an entry here cannot be a claim nobody checked.

The two tables below are the machine-read part. Everything else on this page is for a reader.

## The densities this surface builds

`styling` S4 asks for two, compact and comfortable. This surface builds ONE, and the token
file says so at the top of `internal/webui/src/tokens.css` ("One density (S4)"); building the
second is `S0053-holdfast-frontend-conventions`. The grader runs the whole matrix once per
density named here, reports the set, and names the density it did not measure rather than
reporting an S4 pass it never took.

The second column is how the ENGINE is put into that density. Two sentences are readable:
`the document as served`, which is the honest answer for a surface with one density and no
switch, and `attribute="value" on the document element`, which is how a switch would be
expressed. When the second density is built, it joins this table and the cell count doubles
with no change to the grader.

| density | how it is entered |
|---|---|
| compact | the document as served |

## The components, and every state each of them owes

A COMPONENT here is the group the grader derives, and its name is the grouping key: the
element's tag, its `type` attribute where it has one, and the classes the cascade selects it
by. The key deliberately carries no role - two elements the engine reports with different
roles landing in one group is a refusal the grader owes, and a key that carried the role
could never fire it.

Only `disabled`, `loading` and `error` may be declared not applicable, and only with a
reason. `default`, `hover`, `focus-visible` and `active` are reachable at the engine for
anything the tree calls interactive and the engine puts in the tab order, so they are proved
or the run fails.

`loading` and `error` are not applicable to every component on this surface, and the reason is
the same one each time and is a property of the design rather than an omission: this page
expresses both on the STATUS LINE beside a control - `#msg`, `#search-msg`, `#held-msg`, and
each view's own `data-state` element - never on the control itself, so a control has no
loading or error appearance to render. Those lines are graded by `frontend` F5 and F7 in the
suites beside this one.

| component | state | verdict | why |
|---|---|---|---|
| a.doclink | default | proved |  |
| a.doclink | hover | proved |  |
| a.doclink | focus-visible | proved |  |
| a.doclink | active | proved |  |
| a.doclink | disabled | not applicable | An anchor has no disabled property and the engine offers no disabled state for one; the documentation link is always available. |
| a.doclink | loading | not applicable | The mark links to a document and issues no request the page waits on, so it has no loading appearance. |
| a.doclink | error | not applicable | Nothing the page does can put this link into an error state; a failed navigation is the browser's own report. |
| a.source-offer-link | default | proved |  |
| a.source-offer-link | hover | proved |  |
| a.source-offer-link | focus-visible | proved |  |
| a.source-offer-link | active | proved |  |
| a.source-offer-link | disabled | not applicable | An anchor has no disabled property, and the AGPL section 13 offer may never be made unavailable. |
| a.source-offer-link | loading | not applicable | The offer is rendered into the document server-side and waits on nothing, so it has no loading appearance. |
| a.source-offer-link | error | not applicable | The offer carries no request of its own that could fail; it is text and a link, both already on the page. |
| button | default | proved |  |
| button | hover | proved |  |
| button | focus-visible | proved |  |
| button | active | proved |  |
| button | disabled | proved |  |
| button | loading | not applicable | A control action's progress is written to the status line beside the bar, never onto the button, so the button has no loading appearance. |
| button | error | not applicable | A refused or failed action is reported by the status line beside the bar in the page's own error role, never by the button changing. |
| button.primary | default | proved |  |
| button.primary | hover | proved |  |
| button.primary | focus-visible | proved |  |
| button.primary | active | proved |  |
| button.primary | disabled | proved |  |
| button.primary | loading | not applicable | The rescan request's progress is written to the status line beside the bar, never onto the button, so the button has no loading appearance. |
| button.primary | error | not applicable | A refused or failed rescan is reported by the status line beside the bar in the page's own error role, never by the button changing. |
| button[type=button].hold | default | proved |  |
| button[type=button].hold | hover | proved |  |
| button[type=button].hold | focus-visible | proved |  |
| button[type=button].hold | active | proved |  |
| button[type=button].hold | disabled | proved |  |
| button[type=button].hold | loading | not applicable | The withholding request's outcome is written to the row's own message line, never onto the per-row button, so it has no loading appearance. |
| button[type=button].hold | error | not applicable | A refused withholding is reported by the message line beneath the list in the page's own error role, never by the button changing. |
| input[type=password] | default | proved |  |
| input[type=password] | hover | proved |  |
| input[type=password] | focus-visible | proved |  |
| input[type=password] | active | proved |  |
| input[type=password] | disabled | proved |  |
| input[type=password] | loading | not applicable | The control token is held in the page and sent with an action; the field itself waits on nothing and has no loading appearance. |
| input[type=password] | error | not applicable | A token the daemon refuses is reported by the status line beside the bar, never by the field changing. |
| input[type=search] | default | proved |  |
| input[type=search] | hover | proved |  |
| input[type=search] | focus-visible | proved |  |
| input[type=search] | active | proved |  |
| input[type=search] | disabled | proved |  |
| input[type=search] | loading | not applicable | A ledger search's progress is written to the search status line beside the field, never onto the field, so it has no loading appearance. |
| input[type=search] | error | not applicable | A failed search is reported by the search status line in the page's own error role, never by the field changing. |

## What is NOT in this file, and why

- **No theme axis.** The matrix runs in the default theme only. Both themes are already
  graded for contrast, in both, by the suites beside this one, and doubling every cell for a
  property another grader owns is cost with no new verdict behind it.
- **No claim of WCAG conformance.** `frontend` F1 names the ceiling: automated tooling
  reaches 30-40% of the criteria. What the grader proves is that a state was entered at the
  engine and rendered differently from the default, which is distinguishable - not that it is
  accessible.
- **No components the operator's own actions summon.** The inventory is derived from the
  served document as it loads, which is what C7's grading route reads. A control that only a
  search or a withholding puts on the page joins the inventory the day the derivation is
  widened, and the run fails until this file names it.
