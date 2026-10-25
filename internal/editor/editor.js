// The editor's only JavaScript (docs/HANDOVER.md §10), inlined and allowed by
// its CSP hash. Every form works without it: the server renders two blank rows
// per list and drops rows left blank. With it, blank rows wait behind an
// "Add" button and every row gets a "Remove" button.
(function () {
  "use strict";
  // The browser's time zone, for the hidden tz fields.
  document.querySelectorAll("input[name=tz]").forEach(function (e) {
    e.value = Intl.DateTimeFormat().resolvedOptions().timeZone;
  });
  // "Changes saved" shows once: a reload or a bookmark does not repeat it.
  if (/[?&]saved=1(&|$)/.test(location.search)) {
    history.replaceState(null, "", location.pathname);
  }

  // A list is [data-list] holding [data-row] children and one
  // [data-add] button. data-row is the row's name prefix up to its index
  // ("sel", "l", "p", "p3.r"); data-i is the index; data-blank marks the rows
  // the server added empty.
  function rows(list) {
    return Array.prototype.filter.call(list.children, function (c) {
      return c.hasAttribute("data-row");
    });
  }

  // clear empties a row the way the server reads "blank": no text, nothing
  // ticked, the first option chosen. A cleared row is dropped on save.
  function clear(row) {
    row.querySelectorAll("input,textarea,select").forEach(function (e) {
      if (e.type === "checkbox") e.checked = false;
      else if (e.tagName === "SELECT") e.selectedIndex = 0;
      else if (e.type !== "button" && !e.hasAttribute("data-keep")) e.value = "";
    });
  }

  // touched: the browser put back something typed before a reload.
  function touched(row) {
    return Array.prototype.some.call(row.querySelectorAll("input,textarea,select"), function (e) {
      if (e.type === "checkbox") return e.checked !== e.defaultChecked;
      if (e.tagName === "SELECT") {
        return Array.prototype.some.call(e.options, function (o) { return o.selected !== o.defaultSelected; });
      }
      return e.type !== "hidden" && e.value !== e.defaultValue;
    });
  }

  function renumber(row, prefix, from, to) {
    var re = new RegExp("^" + prefix.replace(/\./g, "\\.") + from + "\\.");
    row.setAttribute("data-i", String(to));
    row.querySelectorAll("[name]").forEach(function (e) {
      e.name = e.name.replace(re, prefix + to + ".");
    });
    row.querySelectorAll("[data-row]").forEach(function (e) {
      e.setAttribute("data-row", e.getAttribute("data-row").replace(re, prefix + to + "."));
    });
  }

  function setup(list) {
    var add = list.querySelector(":scope > [data-add]");
    var all = rows(list);
    var blank = all.filter(function (r) { return r.hasAttribute("data-blank"); });
    if (!add || blank.length === 0) return;
    var pristine = blank[0].cloneNode(true);
    var keepOne = blank.length === all.length; // nothing filled yet: show one
    blank.forEach(function (r, k) {
      if (!(keepOne && k === 0) && !touched(r) && !r.querySelector(".err")) r.hidden = true;
    });
    add.hidden = false;
    add.addEventListener("click", function () {
      var next = rows(list).filter(function (r) { return r.hidden && r.hasAttribute("data-blank"); })[0];
      if (!next) {
        var prefix = pristine.getAttribute("data-row");
        var max = -1;
        rows(list).forEach(function (r) { max = Math.max(max, +r.getAttribute("data-i")); });
        next = pristine.cloneNode(true);
        renumber(next, prefix, pristine.getAttribute("data-i"), max + 1);
        list.insertBefore(next, add);
        wire(next);
      }
      next.removeAttribute("data-blank");
      next.hidden = false;
      var order = next.querySelector("[data-order]");
      if (order) {
        var top = 0;
        rows(list).forEach(function (r) {
          var o = r !== next && !r.hidden && r.querySelector("[data-order]");
          if (o && /^[0-9]+$/.test(o.value)) top = Math.max(top, +o.value);
        });
        order.value = String(top + 1);
      }
      var focus = next.querySelector("input:not([type=hidden]):not([data-order]),textarea");
      if (focus) focus.focus();
    });
  }

  function wire(scope) {
    scope.querySelectorAll("[data-remove]").forEach(function (b) {
      b.hidden = false;
      b.addEventListener("click", function () {
        var row = b.closest("[data-row]");
        clear(row);
        row.hidden = true;
        row.setAttribute("data-blank", ""); // Add reuses it before making a new row
        var add = row.parentNode.querySelector(":scope > [data-add]");
        if (add) add.focus();
      });
    });
    scope.querySelectorAll("[data-list]").forEach(setup);
  }

  // "Copy into the HTML template field": the built-in page's source.
  document.querySelectorAll("[data-fill]").forEach(function (b) {
    var field = b.form && b.form.elements[b.getAttribute("data-fill")];
    var source = document.querySelector("[data-fill-source]");
    if (!field || !source) return;
    b.hidden = false;
    b.addEventListener("click", function () {
      if (field.value.trim() !== "" && !confirm("Replace the HTML template field?")) return;
      field.value = source.textContent;
      field.focus();
    });
  });

  wire(document);
})();
