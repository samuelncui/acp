add a acp-rewrite command:

- scan the whole path, gen tasks of copy file to a tmp file and mv back.
    - eg aaa.mp4 -cp-> aaa.mp4.tmp{random_number} -mv-> aaa.mp4
    - use acp lib to copy file, with all features of acp.
    - keep all file permission, mtime, atime, attrs, xattrs (using the acp's feature).
- only rewrite regular file, skip dir.
- has state storage, can resume from last time.
    - if stoped, cleanup unfinished tmp file.
    - when resume will cleanup unfinished tmp file from the last time if any.
- skip file if the file is being used by other process. store them to do later.
- if a file is hardlinked, only rewrite one of them, and relink the other links.
- find all same files by sha256&size from the acp report, and list them as final report.
