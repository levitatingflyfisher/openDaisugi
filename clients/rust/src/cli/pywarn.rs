//! Python's warnings filters, as far as a UserWarning needs them.

/// The warning categories of Python's builtins, each with whether a
/// UserWarning is an instance of it.
const BUILTIN_WARNINGS: &[(&str, bool)] = &[
    ("Warning", true),
    ("UserWarning", true),
    ("DeprecationWarning", false),
    ("PendingDeprecationWarning", false),
    ("SyntaxWarning", false),
    ("RuntimeWarning", false),
    ("FutureWarning", false),
    ("ImportWarning", false),
    ("UnicodeWarning", false),
    ("BytesWarning", false),
    ("ResourceWarning", false),
    ("EncodingWarning", false),
];

/// Whether Python prints a UserWarning with text `msg`, raised from a
/// module this binary cannot name, under the filters PYTHONWARNINGS sets
/// (`warnings._processoptions` over the default filters, which show it).
/// None for a setting whose effect this binary does not model: an invalid
/// option (Python prints a note at start), the "error" action (the
/// warning becomes an exception), a filter that names a module or a line,
/// or a category that is not one of the builtins. The Go client's
/// `userWarningShown` is the reference.
pub fn user_warning_shown(python_warnings: &str, msg: &str) -> Option<bool> {
    if python_warnings.is_empty() {
        return Some(true);
    }
    let mut filters: Vec<(&str, String)> = vec![];
    for opt in python_warnings.split(',') {
        let mut parts: Vec<&str> = opt.split(':').map(str::trim).collect();
        if parts.len() > 5 {
            return None;
        }
        parts.resize(5, "");
        let (action, message, category, module, lineno) = (parts[0], parts[1], parts[2], parts[3], parts[4]);
        let action = match action {
            "" => "default",
            "all" => "always",
            a => ["default", "always", "ignore", "module", "once", "error"].into_iter().find(|x| x.starts_with(a))?,
        };
        if !module.is_empty() || !(lineno.is_empty() || lineno == "0") {
            return None;
        }
        let category = if category.is_empty() { "Warning" } else { category };
        let category = category.strip_prefix("builtins.").unwrap_or(category);
        let (_, is_user) = BUILTIN_WARNINGS.iter().find(|(n, _)| *n == category)?;
        // A filter that shows a category beyond UserWarning shows the
        // warnings Python's libraries raise too (DeprecationWarning and
        // the rest), which this binary cannot know.
        if category != "UserWarning" && action != "ignore" {
            return None;
        }
        if *is_user {
            filters.push((action, message.to_string()));
        }
    }
    // Each option goes to the front of the filters, so the last one that
    // matches wins. The message is a case-blind prefix (re.escape, re.I).
    for (action, message) in filters.iter().rev() {
        let lower: String = msg.chars().flat_map(char::to_lowercase).collect();
        let want: String = message.chars().flat_map(char::to_lowercase).collect();
        if !lower.starts_with(&want) {
            continue;
        }
        if *action == "error" {
            return None;
        }
        return Some(*action != "ignore");
    }
    Some(true)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reads_pythonwarnings_as_python_does() {
        assert_eq!(user_warning_shown("", "x"), Some(true));
        assert_eq!(user_warning_shown("ignore::UserWarning", "x"), Some(false));
        assert_eq!(user_warning_shown("ignore", "x"), Some(false));
        assert_eq!(user_warning_shown("i::DeprecationWarning", "x"), Some(true));
        assert_eq!(user_warning_shown("ignore::UserWarning,default::UserWarning", "x"), Some(true));
        assert_eq!(user_warning_shown("default", "x"), None);
        assert_eq!(user_warning_shown("always::DeprecationWarning", "x"), None);
        assert_eq!(user_warning_shown("ignore:3 PATH", "3 pathway(s)"), Some(false));
        assert_eq!(user_warning_shown("ignore:4", "3 pathway(s)"), Some(true));
        assert_eq!(user_warning_shown("error", "x"), None);
        assert_eq!(user_warning_shown("bogus", "x"), None);
        assert_eq!(user_warning_shown("ignore::NoSuchWarning", "x"), None);
        assert_eq!(user_warning_shown("ignore:::m", "x"), None);
    }
}
