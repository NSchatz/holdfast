// The DOM primitives, plus the ONE drawing primitive two different views now build from.
// Everything the page puts on screen is built here or through here, and there is exactly
// one way to do it: createElement plus textContent. No module in
// this directory assigns a string to an HTML sink, which is what makes the response
// Content-Security-Policy's `require-trusted-types-for 'script'` a guarantee rather than
// a hope - a regression that string-built from an attacker-influencable media path would
// throw in the browser instead of silently reintroducing a sink.
const $ = (id) => document.getElementById(id);

// mk(tag, class, text): the one DOM-node factory. textContent means any string it is
// handed - a media path, a failure reason - is inert text, never parsed as markup.
function mk(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}
// The honest "not recorded" node for a nil/absent outcome field.
function nrNode() { return mk("span", "nr", NOT_RECORDED); }

// tplNode clones one shell out of a <template> in the document. It is how every row is
// built, and it is also the ONLY way a vector element gets onto this page: the SVG
// namespace comes from the HTML parser reading the template, so no module here names a
// namespace URI, imports a drawing library, or builds an element from a string.
function tplNode(id) { return $(id).content.cloneNode(true).firstElementChild; }

// figBar is a proportional bar: a shell cloned from the document's own <template> with
// ONE geometry attribute set from a number that was already measured elsewhere. It draws
// nothing it computes itself.
//
// It lives here, with the other primitives, because two views draw one: a distribution's
// buckets in the whole-ledger cards, and how far through its source a running encode is.
// The rules are the same in both places and are the drawings' own (60-aggregates.js): no
// library, no image, no font, no data: URI, no string assigned to an HTML sink; nothing
// told apart by colour alone; and every value it encodes rendered as text beside it, so
// removing every drawing from the page costs a reader no number at all.
function figBar(share) {
  const svg = tplNode("tpl-fig-bar");
  svg.querySelector(".mark").setAttribute("width", (share * 100).toFixed(4) + "%");
  return svg;
}
