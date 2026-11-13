# schwabfile's fixtures

These files are schwab.com's exports, and the files of schwabtx that the
store imports, in the shapes that `Read` knows, written by hand. Every
account number, name, symbol, CUSIP and amount in them is made up: the
accounts end in 1234, 123 or 456, the securities are XYZ, ABCD and the like,
and the CUSIPs are `000000AA7` and its kin, which no issuer holds. None is
copied from a real file or from another project's; each is modeled on what
the research found of the layout it stands for, which its entry's `source`
names. `.gitattributes` keeps their bytes, line ends, byte order marks and
Windows-1252 included, in every checkout.

`index.json` has one entry per file, but itself, this README and the
`golden` and `fuzz` directories: the file, the name an export of it would
have (which `Read` takes the account's digits and the time of the export
from), where it is modeled from, how many carriage returns it holds and
whether it starts with a byte order mark. The files under `refuse/` are
Schwab's other files, and other files, that `Read` refuses; their entries
say what the refusal names the file as instead of a name.

`golden/` holds what `Read` makes of each file that it reads, as
`TestReadFixtures` records it. After a change to the readers, rewrite them
with

    go test ./internal/history/schwabfile -run TestReadFixtures -update

and read the diff before committing it.
