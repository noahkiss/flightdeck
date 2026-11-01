import Foundation

/// Word error rate: (substitutions + deletions + insertions) / reference words,
/// after both texts are normalised.
enum WordErrorRate {
    struct Score {
        let errors: Int
        let referenceWords: Int
        var rate: Double { referenceWords == 0 ? 0 : Double(errors) / Double(referenceWords) }
    }

    /// Lowercases, maps a few symbols to the words people say for them, drops other
    /// punctuation, and splits on whitespace. Digits become words so "3" and "three" match.
    static func normalise(_ text: String) -> [String] {
        var s = text.lowercased()
        let symbols: [(String, String)] = [
            ("/", " slash "), ("_", " underscore "), ("-", " dash "), (".", " dot "),
            ("=", " equals "), ("@", " at "), ("#", " hash "), ("+", " plus "),
        ]
        // Sentence-final full stops are punctuation, not "dot"; strip them first.
        s = s.replacingOccurrences(of: #"\.(\s|$)"#, with: "$1", options: .regularExpression)
        // A dash between spaces is a pause, not a spoken "dash".
        s = s.replacingOccurrences(of: #"\s-+\s"#, with: " ", options: .regularExpression)
        for (symbol, word) in symbols {
            s = s.replacingOccurrences(of: symbol, with: word)
        }
        let digits = ["zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine"]
        for (digit, word) in digits.enumerated() {
            s = s.replacingOccurrences(of: String(digit), with: " \(word) ")
        }
        let allowed = CharacterSet.lowercaseLetters.union(.whitespaces).union(CharacterSet(charactersIn: "'"))
        s = String(s.unicodeScalars.map { allowed.contains($0) ? Character($0) : " " })
        s = s.replacingOccurrences(of: "'", with: "")
        return s.split(whereSeparator: { $0.isWhitespace }).map(String.init)
    }

    static func score(reference: String, hypothesis: String) -> Score {
        let ref = normalise(reference)
        let hyp = normalise(hypothesis)
        if ref.isEmpty { return Score(errors: hyp.count, referenceWords: 0) }
        if hyp.isEmpty { return Score(errors: ref.count, referenceWords: ref.count) }
        var previous = Array(0...hyp.count)
        var current = [Int](repeating: 0, count: hyp.count + 1)
        for i in 1...ref.count {
            current[0] = i
            for j in 1...hyp.count {
                let cost = ref[i - 1] == hyp[j - 1] ? 0 : 1
                current[j] = min(previous[j] + 1, current[j - 1] + 1, previous[j - 1] + cost)
            }
            swap(&previous, &current)
        }
        return Score(errors: previous[hyp.count], referenceWords: ref.count)
    }
}
