# Sheets

**Sheets** is a lightweight spreadsheet app for everyday tables — budgets, inventories, lists — compatible with
Excel and other spreadsheet programs through XLSX, XLS and CSV.

## Workbooks and sheets

A workbook has one or more sheets (tabs). New workbooks start with "Sheet1" (100 rows × 26 columns); grids grow
as you need. You can add, rename, reorder and delete sheets (except the last one), resize columns and rows,
freeze header rows/columns, insert and delete rows and columns (formulas are adjusted), and **sort** a range by
a column.

## Editing and formatting

Click a cell and type. Entries starting with `=` are formulas. Formatting from the toolbar or shortcuts:

* **bold** (`Ctrl+B`), *italic* (`Ctrl+I`), underline (`Ctrl+U`), strikethrough,
* text colour and **fill/highlight** colour,
* horizontal and vertical alignment, wrap text, font size, borders,
* number formats: general, `0`, `0.00`, `#,##0`, `#,##0.00`, percent, currency `$#,##0.00`, date `yyyy-mm-dd`,
  text `@`, or a custom Excel-style format.

Formatting applies to the selection, so you can highlight whole ranges at once. Copy/paste works with other
spreadsheet programs as tab-separated values. Changes save automatically and dependent cells recalculate
instantly, including across sheets.

## Formulas

Operators: `+ - * / ^` (power), `&` (text concatenation), comparisons `= <> < > <= >=`, unary minus and percent
(`50%`). References: `A1`, absolute `$A$1`, ranges `A1:B5`, whole columns `A:A` and rows `1:1`, other sheets
`Sheet2!A1` and `'My Sheet'!A1:B2`.

| Group | Functions |
|---|---|
| Math | `SUM`, `PRODUCT`, `ROUND`, `ROUNDUP`, `ROUNDDOWN`, `INT`, `ABS`, `SQRT`, `POWER`, `MOD`, `PI`, `RAND` |
| Statistics | `AVERAGE`, `MEDIAN`, `MIN`, `MAX`, `COUNT`, `COUNTA`, `COUNTBLANK` |
| Conditional | `COUNTIF`, `SUMIF`, `AVERAGEIF`, `IF`, `IFERROR` |
| Logic | `AND`, `OR`, `NOT` |
| Text | `CONCAT`, `CONCATENATE`, `TEXTJOIN`, `LEN`, `UPPER`, `LOWER`, `PROPER`, `TRIM`, `LEFT`, `RIGHT`, `MID`, `SUBSTITUTE`, `FIND`, `SEARCH`, `TEXT`, `VALUE` |
| Date | `NOW`, `TODAY`, `DATE`, `YEAR`, `MONTH`, `DAY` |
| Lookup | `VLOOKUP`, `HLOOKUP`, `INDEX`, `MATCH` |
| Information | `ISBLANK`, `ISNUMBER`, `ISTEXT` |

The live list with syntax and descriptions is at `GET /api/v1/sheets/functions` and in the formula bar's
autocomplete. Errors: `#DIV/0!`, `#VALUE!`, `#REF!` (reference to a deleted cell), `#NAME?` (unknown function),
`#N/A`, `#NUM!` and `#CIRC!` (circular reference). Dates are stored as Excel serial numbers (1900 system) and
shown according to the cell's number format. `RAND()` produces a new value on each recalculation.

Limits: 1 000 000 cells per workbook, 32 000 characters per cell, formulas nested at most 64 levels deep.

## Import and export

* **Import** an `.xlsx`, `.xls` or `.csv` file (up to 50 MiB) from your computer, or straight from Files
  (right-click a spreadsheet → *Open in Sheets*). Values, formulas and basic styles (bold, italic, underline,
  colours, fills, alignment, number formats, column widths, frozen panes) are kept; features Sheets does not
  support (charts, pivot tables, macros, conditional formatting) are dropped. CSV delimiters and a UTF-8 BOM
  are detected automatically.
* **Export** as XLSX, XLS or CSV (CSV exports one sheet), either as a download or saved into a folder in Files.

Imported files are parsed with strict limits on size, sheet count and nesting, and macros are never executed.

## Using Sheets from scripts and AI agents

Every action in the app is available through the API, and the cell-level endpoints are designed to be easy for
programs — including AI assistants — to use safely:

| Task | Endpoint |
|---|---|
| Find a workbook | `GET /api/v1/sheets?q=budget` |
| List its sheets | `GET /api/v1/sheets/{id}` (or the summary from the list) |
| Read a block as 2-D arrays (values, formulas, styles) | `GET /api/v1/sheets/{id}/sheets/{sheet_id}/range?ref=A1:D20` |
| Write a block (strings starting with `=` are formulas) | `PUT /api/v1/sheets/{id}/sheets/{sheet_id}/range` |
| Edit individual cells, styles, or style a range | `PATCH /api/v1/sheets/{id}/sheets/{sheet_id}/cells` |
| Try a formula without saving it | `POST /api/v1/sheets/evaluate` |
| Insert/delete rows or columns, sort | `POST …/insert`, `…/delete`, `…/sort` |

Write responses return every changed cell, including recalculated dependents, so an agent can verify the
effect of its edit without re-reading the sheet. Example — append a total row and make it bold:

```bash
curl -X PATCH "https://nas.local:8443/api/v1/sheets/$WB/sheets/$SHEET/cells" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"updates":[{"ref":"A10","value":"Total"},{"ref":"B10","formula":"=SUM(B2:B9)"}],
       "range_style":{"ref":"A10:B10","style":{"b":true,"bg":"#fff2cc"}}}'
```

Give agents a [personal access token](../api/authentication.md#personal-access-tokens) with only
`sheets:read` (or `sheets:read sheets:write`) so they cannot touch anything else. The
[Python SDK](../clients/python.md) wraps all of this (`lw.sheets.get_range`, `set_range`, `update_cells`,
`style_range`, …).
