package osfs

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

type outputSessionIDSequence struct {
	mu  sync.Mutex
	ids []transfer.OutputSessionID
}

func TestOutputFilenameNamespaceIsCaseInsensitiveInjective(t *testing.T) {
	var upperEncoding, lowerEncoding [transfer.OutputSessionIdentityBytes]byte
	upperEncoding[len(upperEncoding)-1] = 1
	lowerEncoding = upperEncoding
	lowerEncoding[0] = 26 << 2
	if !strings.EqualFold(
		base64.RawURLEncoding.EncodeToString(upperEncoding[:]),
		base64.RawURLEncoding.EncodeToString(lowerEncoding[:]),
	) {
		t.Fatal("fixture no longer demonstrates the base64url case collision")
	}
	upperToken := encodeOutputFilenameToken(upperEncoding[:])
	lowerToken := encodeOutputFilenameToken(lowerEncoding[:])
	if strings.EqualFold(upperToken, lowerToken) || strings.ToLower(upperToken) != upperToken || strings.ToLower(lowerToken) != lowerToken {
		t.Fatalf("case-insensitive output tokens collide: %q %q", upperToken, lowerToken)
	}
	name := outputJournalPrefix + upperToken + ".journal"
	if _, ok := outputSessionIDFromJournalName(name); !ok {
		t.Fatalf("canonical output journal name rejected: %q", name)
	}
	if _, ok := outputSessionIDFromJournalName(strings.ToUpper(name)); ok {
		t.Fatal("non-canonical case variant admitted as a separate authority name")
	}
}

func TestOutputNamespaceRejectsNonCanonicalNames(t *testing.T) {
	validStage := outputStagePrefix + strings.Repeat("01", outputStageRandomBytes)
	for _, name := range []string{
		"",
		filepath.Join("child", validStage),
		"not-a-stage",
		outputStagePrefix + "not-hex",
		outputStagePrefix + "01",
		strings.ToUpper(validStage),
	} {
		if validOutputStageName(name) {
			t.Fatalf("non-canonical stage name accepted: %q", name)
		}
	}
	if !validOutputStageName(validStage) {
		t.Fatalf("canonical stage name rejected: %q", validStage)
	}

	var sessionBytes [transfer.OutputSessionIdentityBytes]byte
	sessionBytes[0] = 1
	validJournal := outputJournalPrefix + encodeOutputFilenameToken(sessionBytes[:]) + ".journal"
	for _, name := range []string{
		"",
		strings.TrimSuffix(validJournal, ".journal"),
		outputJournalPrefix + "not-hex.journal",
		outputJournalPrefix + "01.journal",
		strings.ToUpper(validJournal),
	} {
		if _, ok := outputSessionIDFromJournalName(name); ok {
			t.Fatalf("non-canonical journal name accepted: %q", name)
		}
	}
	zeroJournal := outputJournalPrefix + strings.Repeat("00", transfer.OutputSessionIdentityBytes) + ".journal"
	if _, ok := outputSessionIDFromJournalName(zeroJournal); ok {
		t.Fatal("zero output session identity accepted")
	}
	if _, ok := outputSessionIDFromJournalName(validJournal); !ok {
		t.Fatalf("canonical journal name rejected: %q", validJournal)
	}

	if _, err := decodeOutputBytes("not-base64", 1); !errors.Is(err, ErrOutputJournalCorrupt) {
		t.Fatalf("invalid base64 error=%v", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte{1})
	if _, err := decodeOutputBytes(encoded, 2); !errors.Is(err, ErrOutputJournalCorrupt) {
		t.Fatalf("wrong decoded length error=%v", err)
	}
}

func TestPathDiagnosticsRemainBoundedAndCategorized(t *testing.T) {
	cause := syscall.ENAMETOOLONG
	failure := filesystemPathFailure("open output", "secret\nname", cause)
	if !errors.Is(failure, ErrPathTooLong) || !errors.Is(failure, cause) {
		t.Fatalf("path failure lost category or cause: %v", failure)
	}
	message := failure.Error()
	if strings.Contains(message, "secret\nname") || !strings.Contains(message, `"secret\nname"`) {
		t.Fatalf("path diagnostic was not safely quoted: %q", message)
	}

	plainCause := errors.New("permission denied")
	plain := filesystemPathFailure("open output", "plain", plainCause)
	if errors.Is(plain, ErrPathTooLong) || !errors.Is(plain, plainCause) {
		t.Fatalf("uncategorized path failure changed semantics: %v", plain)
	}
	if got := plain.Error(); got != `open output "plain": operation failed` {
		t.Fatalf("uncategorized diagnostic=%q", got)
	}

	longPath := strings.Repeat("界", maximumPathDiagnosticBytes)
	quoted := quotePathForDiagnostic(longPath)
	if len(quoted) > maximumPathDiagnosticBytes || !strings.Contains(quoted, "…") {
		t.Fatalf("long diagnostic was not bounded at a rune boundary: length=%d value=%q", len(quoted), quoted)
	}
}

func TestOutputSessionLockCloseIsIdempotent(t *testing.T) {
	var absent *outputSessionLock
	if err := absent.close(true); err != nil {
		t.Fatalf("nil lock close error=%v", err)
	}

	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	for _, removeBeforeClose := range []bool{false, true} {
		lock, err := acquireOutputSessionLock(root, "close-test.lock")
		if err != nil {
			t.Fatal(err)
		}
		if removeBeforeClose {
			if err := root.Remove(lock.name); err != nil {
				t.Fatal(err)
			}
		}
		if err := lock.close(true); err != nil {
			t.Fatalf("close(remove=%v) error=%v", removeBeforeClose, err)
		}
		if err := lock.close(true); err != nil {
			t.Fatalf("repeated close(remove=%v) error=%v", removeBeforeClose, err)
		}
	}
}

func (s *outputSessionIDSequence) NewOutputSessionID() (transfer.OutputSessionID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ids) == 0 {
		return transfer.OutputSessionID{}, errors.New("test output session IDs exhausted")
	}
	result := s.ids[0]
	s.ids = s.ids[1:]
	return result, nil
}

func outputAuthorityFor(t *testing.T, ids ...byte) *FilesystemOutputAuthority {
	t.Helper()
	sequence := &outputSessionIDSequence{}
	for _, value := range ids {
		sequence.ids = append(sequence.ids, outputTestID[transfer.OutputSessionID](value))
	}
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{SessionIDs: sequence})
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func outputDiscoveryIntent(config FilesystemOutputSessionConfig) FilesystemOutputIntent {
	return FilesystemOutputIntent{
		RootPath: config.RootPath, ShareInstance: config.ShareInstance, ResumeIntent: config.ResumeIntent,
	}
}

func TestFilesystemOutputAuthorityDiscoversSessionAndPreservesVerifiedRanges(t *testing.T) {
	config := outputTestConfig(t.TempDir())
	intent := outputDiscoveryIntent(config)
	created, err := outputAuthorityFor(t, 21).OpenOrCreate(context.Background(), intent)
	if err != nil || created.Reopened || created.Session.SessionID() != outputTestID[transfer.OutputSessionID](21) {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	descriptor := outputTestDescriptor(t, config, 1, 2, 64, catalog.ModifiedTime{})
	transaction, _, err := created.Session.BeginFile(context.Background(), outputTestFile("resume.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 0, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	crashOutputSession(created.Session, transaction)

	reopened, err := outputAuthorityFor(t, 22).OpenOrCreate(context.Background(), intent)
	if err != nil || !reopened.Reopened || reopened.Session.SessionID() != created.Session.SessionID() {
		t.Fatalf("reopened=%+v err=%v", reopened, err)
	}
	resumed, durable, err := reopened.Session.BeginFile(context.Background(), outputTestFile("resume.bin", descriptor))
	if err != nil || len(durable.Ranges().Ranges()) != 1 || durable.Ranges().Ranges()[0].End != 32 {
		t.Fatalf("durable=%v err=%v", durable.Ranges().Ranges(), err)
	}
	_, _ = resumed.Abort(context.Background(), errors.New("cleanup"))
	_ = reopened.Session.AbortJob(context.Background(), errors.New("cleanup"))
}

func TestFilesystemOutputAuthorityTreatsWindowsCaseAliasesAsOneRoot(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows root aliases are platform-specific")
	}
	root := filepath.Join(t.TempDir(), "MixedCaseRoot")
	if err := os.Mkdir(root, dirPerm); err != nil {
		t.Fatal(err)
	}
	config := outputTestConfig(root)
	first, err := NewFilesystemOutputSession(config)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := first.SessionID()
	crashOutputSession(first)
	intent := outputDiscoveryIntent(config)
	intent.RootPath = strings.ToUpper(root)
	opened, err := outputAuthorityFor(t, 25).OpenOrCreate(context.Background(), intent)
	if err != nil || !opened.Reopened || opened.Quarantined != 0 || opened.Session.SessionID() != sessionID {
		t.Fatalf("case-alias discovery=%+v err=%v", opened, err)
	}
	_ = opened.Session.AbortJob(context.Background(), errors.New("cleanup"))
}

func TestFilesystemOutputAuthorityKeepsForeignIntentAndShareJournals(t *testing.T) {
	root := t.TempDir()
	firstConfig := outputTestConfig(root)
	first, err := NewFilesystemOutputSession(firstConfig)
	if err != nil {
		t.Fatal(err)
	}
	firstJournal := first.journalName
	crashOutputSession(first)

	secondIntent := outputDiscoveryIntent(firstConfig)
	secondIntent.ResumeIntent[0]++
	second, err := outputAuthorityFor(t, 31).OpenOrCreate(context.Background(), secondIntent)
	if err != nil || second.Reopened || second.Quarantined != 0 {
		t.Fatalf("different intent open=%+v err=%v", second, err)
	}
	if _, err := os.Stat(filepath.Join(root, firstJournal)); err != nil {
		t.Fatalf("foreign intent journal was removed: %v", err)
	}
	_ = second.Session.AbortJob(context.Background(), errors.New("cleanup"))

	first, err = NewFilesystemOutputSession(firstConfig)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.AbortJob(context.Background(), errors.New("cleanup"))

	thirdConfig := outputTestConfig(root)
	thirdConfig.SessionID = outputTestID[transfer.OutputSessionID](32)
	thirdConfig.ShareInstance = outputTestID[catalog.ShareInstance](33)
	third, err := NewFilesystemOutputSession(thirdConfig)
	if err != nil {
		t.Fatal(err)
	}
	thirdJournal := third.journalName
	crashOutputSession(third)
	fourth, err := outputAuthorityFor(t, 34).OpenOrCreate(context.Background(), outputDiscoveryIntent(firstConfig))
	if err != nil || fourth.Reopened || fourth.Quarantined != 0 {
		t.Fatalf("different share open=%+v err=%v", fourth, err)
	}
	if _, err := os.Stat(filepath.Join(root, thirdJournal)); err != nil {
		t.Fatalf("foreign share journal was removed: %v", err)
	}
	_ = fourth.Session.AbortJob(context.Background(), errors.New("cleanup"))
	third, _ = NewFilesystemOutputSession(thirdConfig)
	_ = third.AbortJob(context.Background(), errors.New("cleanup"))
}

func TestFilesystemOutputAuthorityQuarantinesCorruptAndTransplantedJournals(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		root := t.TempDir()
		config := outputTestConfig(root)
		staleID := outputTestID[transfer.OutputSessionID](41)
		staleName := outputJournalPrefix + encodeOutputFilenameToken(staleID.Bytes()) + ".journal"
		if err := os.WriteFile(filepath.Join(root, staleName), []byte("corrupt"), filePerm); err != nil {
			t.Fatal(err)
		}
		opened, err := outputAuthorityFor(t, 42).OpenOrCreate(context.Background(), outputDiscoveryIntent(config))
		if err != nil || opened.Quarantined != 1 || opened.Reopened {
			t.Fatalf("opened=%+v err=%v", opened, err)
		}
		if _, err := os.Stat(filepath.Join(root, staleName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("corrupt journal remained at authoritative name: %v", err)
		}
		matches, _ := filepath.Glob(filepath.Join(root, outputQuarantinePrefix+"*.journal"))
		if len(matches) != 1 {
			t.Fatalf("quarantine files=%v", matches)
		}
		_ = opened.Session.AbortJob(context.Background(), errors.New("cleanup"))
	})

	t.Run("transplanted root", func(t *testing.T) {
		parent := t.TempDir()
		sourceRoot := filepath.Join(parent, "source")
		targetRoot := filepath.Join(parent, "target")
		if err := os.MkdirAll(sourceRoot, dirPerm); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(targetRoot, dirPerm); err != nil {
			t.Fatal(err)
		}
		config := outputTestConfig(sourceRoot)
		source, err := NewFilesystemOutputSession(config)
		if err != nil {
			t.Fatal(err)
		}
		journalName := source.journalName
		crashOutputSession(source)
		journal, err := os.ReadFile(filepath.Join(sourceRoot, journalName))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(targetRoot, journalName), journal, filePerm); err != nil {
			t.Fatal(err)
		}
		targetIntent := outputDiscoveryIntent(config)
		targetIntent.RootPath = targetRoot
		opened, err := outputAuthorityFor(t, 43).OpenOrCreate(context.Background(), targetIntent)
		if err != nil || opened.Quarantined != 1 || opened.Reopened {
			t.Fatalf("opened=%+v err=%v", opened, err)
		}
		_ = opened.Session.AbortJob(context.Background(), errors.New("cleanup"))
		source, _ = NewFilesystemOutputSession(config)
		_ = source.AbortJob(context.Background(), errors.New("cleanup"))
	})
}

func TestFilesystemOutputAuthorityRejectsActiveAndAmbiguousSessions(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		config := outputTestConfig(t.TempDir())
		intent := outputDiscoveryIntent(config)
		active, err := outputAuthorityFor(t, 51).OpenOrCreate(context.Background(), intent)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := outputAuthorityFor(t, 52).OpenOrCreate(context.Background(), intent); !errors.Is(err, ErrOutputSessionActive) {
			t.Fatalf("active discovery error=%v", err)
		}
		_ = active.Session.AbortJob(context.Background(), errors.New("cleanup"))
	})

	t.Run("ambiguous", func(t *testing.T) {
		root := t.TempDir()
		config := outputTestConfig(root)
		first, err := NewFilesystemOutputSession(config)
		if err != nil {
			t.Fatal(err)
		}
		crashOutputSession(first)
		rootHandle, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		document, err := readOutputJournalAt(rootHandle, first.journalName)
		if err != nil {
			t.Fatal(err)
		}
		secondID := outputTestID[transfer.OutputSessionID](53)
		document.OutputSession = encodeOutputBytes(secondID.Bytes())
		secondName := outputJournalPrefix + encodeOutputFilenameToken(secondID.Bytes()) + ".journal"
		if _, err := persistOutputJournal(rootHandle, secondName, document, nil); err != nil {
			t.Fatal(err)
		}
		_ = rootHandle.Close()
		if _, err := outputAuthorityFor(t, 54).OpenOrCreate(context.Background(), outputDiscoveryIntent(config)); !errors.Is(err, ErrOutputDiscoveryAmbiguous) {
			t.Fatalf("ambiguous discovery error=%v", err)
		}
		first, _ = NewFilesystemOutputSession(config)
		_ = first.AbortJob(context.Background(), errors.New("cleanup"))
		secondConfig := config
		secondConfig.SessionID = secondID
		second, _ := NewFilesystemOutputSession(secondConfig)
		_ = second.AbortJob(context.Background(), errors.New("cleanup"))
	})
}

func TestFilesystemOutputAuthorityBoundsDiscoveryBeforeMutation(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	var firstName string
	for index := 0; index <= MaxOutputJournalCandidates; index++ {
		var raw [transfer.OutputSessionIdentityBytes]byte
		raw[0] = 1
		raw[len(raw)-2] = byte(index >> 8)
		raw[len(raw)-1] = byte(index)
		name := outputJournalPrefix + encodeOutputFilenameToken(raw[:]) + ".journal"
		if index == 0 {
			firstName = name
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte("corrupt"), filePerm); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := outputAuthorityFor(t, 61).OpenOrCreate(context.Background(), outputDiscoveryIntent(config)); !errors.Is(err, ErrOutputDiscoveryLimit) {
		t.Fatalf("discovery limit error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, firstName)); err != nil {
		t.Fatalf("limit failure mutated a candidate: %v", err)
	}
}

func TestOutputResumeIntentValidationAndDefensiveAuthorityInputs(t *testing.T) {
	if _, err := OutputResumeIntentFromBytes(nil); err == nil {
		t.Fatal("empty resume intent accepted")
	}
	if _, err := OutputResumeIntentFromBytes(make([]byte, OutputResumeIntentBytes)); err == nil {
		t.Fatal("zero resume intent accepted")
	}
	raw := make([]byte, OutputResumeIntentBytes)
	raw[0] = 1
	intent, err := OutputResumeIntentFromBytes(raw)
	if err != nil || intent.IsZero() || intent.Bytes()[0] != 1 {
		t.Fatalf("intent=%v err=%v", intent, err)
	}
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.OpenOrCreate(context.Background(), FilesystemOutputIntent{}); err == nil {
		t.Fatal("empty discovery intent accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	config := outputTestConfig(t.TempDir())
	if _, err := authority.OpenOrCreate(canceled, outputDiscoveryIntent(config)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discovery error=%v", err)
	}
}

func TestFilesystemOutputAuthorityCreationFailureDomains(t *testing.T) {
	t.Run("default cryptographic identity", func(t *testing.T) {
		config := outputTestConfig(t.TempDir())
		authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{})
		if err != nil {
			t.Fatal(err)
		}
		opened, err := authority.OpenOrCreate(context.Background(), outputDiscoveryIntent(config))
		if err != nil || opened.Session.SessionID().IsZero() {
			t.Fatalf("cryptographic session=%+v err=%v", opened, err)
		}
		_ = opened.Session.AbortJob(context.Background(), errors.New("cleanup"))
	})

	t.Run("zero identity retry", func(t *testing.T) {
		config := outputTestConfig(t.TempDir())
		opened, err := outputAuthorityFor(t, 0, 71).OpenOrCreate(context.Background(), outputDiscoveryIntent(config))
		if err != nil || opened.Session.SessionID() != outputTestID[transfer.OutputSessionID](71) {
			t.Fatalf("retried session=%+v err=%v", opened, err)
		}
		_ = opened.Session.AbortJob(context.Background(), errors.New("cleanup"))
	})

	t.Run("foreign identity collision retry", func(t *testing.T) {
		root := t.TempDir()
		foreignConfig := outputTestConfig(root)
		foreignConfig.SessionID = outputTestID[transfer.OutputSessionID](72)
		foreign, err := NewFilesystemOutputSession(foreignConfig)
		if err != nil {
			t.Fatal(err)
		}
		crashOutputSession(foreign)
		intent := outputDiscoveryIntent(foreignConfig)
		intent.ResumeIntent[0]++
		opened, err := outputAuthorityFor(t, 72, 73).OpenOrCreate(context.Background(), intent)
		if err != nil || opened.Session.SessionID() != outputTestID[transfer.OutputSessionID](73) {
			t.Fatalf("collision retry=%+v err=%v", opened, err)
		}
		_ = opened.Session.AbortJob(context.Background(), errors.New("cleanup"))
		foreign, _ = NewFilesystemOutputSession(foreignConfig)
		_ = foreign.AbortJob(context.Background(), errors.New("cleanup"))
	})

	t.Run("generator error and exhaustion", func(t *testing.T) {
		config := outputTestConfig(t.TempDir())
		sentinel := errors.New("identity source unavailable")
		authority, _ := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
			SessionIDs: OutputSessionIDGeneratorFunc(func() (transfer.OutputSessionID, error) {
				return transfer.OutputSessionID{}, sentinel
			}),
		})
		if _, err := authority.OpenOrCreate(context.Background(), outputDiscoveryIntent(config)); !errors.Is(err, sentinel) {
			t.Fatalf("identity source error=%v", err)
		}

		calls := 0
		authority, _ = NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
			SessionIDs: OutputSessionIDGeneratorFunc(func() (transfer.OutputSessionID, error) {
				calls++
				return transfer.OutputSessionID{}, nil
			}),
		})
		if _, err := authority.OpenOrCreate(context.Background(), outputDiscoveryIntent(config)); err == nil || calls != outputNamespaceAllocationAttempts {
			t.Fatalf("identity exhaustion calls=%d err=%v", calls, err)
		}
	})
}

func TestFilesystemOutputAuthorityFailsClosedOnUnsafeJournalObjects(t *testing.T) {
	t.Run("invalid authority name", func(t *testing.T) {
		root := t.TempDir()
		config := outputTestConfig(root)
		name := outputJournalPrefix + "not-a-canonical-session" + ".journal"
		if err := os.WriteFile(filepath.Join(root, name), []byte("corrupt"), filePerm); err != nil {
			t.Fatal(err)
		}
		opened, err := outputAuthorityFor(t, 81).OpenOrCreate(context.Background(), outputDiscoveryIntent(config))
		if err != nil || opened.Quarantined != 1 {
			t.Fatalf("invalid-name quarantine=%+v err=%v", opened, err)
		}
		_ = opened.Session.AbortJob(context.Background(), errors.New("cleanup"))
	})

	t.Run("active corrupt session", func(t *testing.T) {
		root := t.TempDir()
		config := outputTestConfig(root)
		active, err := NewFilesystemOutputSession(config)
		if err != nil {
			t.Fatal(err)
		}
		journalPath := filepath.Join(root, active.journalName)
		if err := os.WriteFile(journalPath, []byte("corrupt"), filePerm); err != nil {
			t.Fatal(err)
		}
		if _, err := outputAuthorityFor(t, 82).OpenOrCreate(context.Background(), outputDiscoveryIntent(config)); !errors.Is(err, ErrOutputDiscoveryUnsafe) || !errors.Is(err, ErrOutputSessionActive) {
			t.Fatalf("active corrupt journal error=%v", err)
		}
		if _, err := os.Stat(journalPath); err != nil {
			t.Fatalf("active journal was quarantined: %v", err)
		}
		_ = active.AbortJob(context.Background(), errors.New("cleanup"))
	})

	t.Run("journal symlink", func(t *testing.T) {
		root := t.TempDir()
		config := outputTestConfig(root)
		target := filepath.Join(root, "journal-target")
		if err := os.WriteFile(target, []byte("not a journal"), filePerm); err != nil {
			t.Fatal(err)
		}
		candidate := outputTestID[transfer.OutputSessionID](83)
		name := outputJournalPrefix + encodeOutputFilenameToken(candidate.Bytes()) + ".journal"
		if err := os.Symlink("journal-target", filepath.Join(root, name)); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		opened, err := outputAuthorityFor(t, 84).OpenOrCreate(context.Background(), outputDiscoveryIntent(config))
		if err != nil || opened.Quarantined != 1 {
			t.Fatalf("symlink quarantine=%+v err=%v", opened, err)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != "not a journal" {
			t.Fatalf("symlink target changed=%q err=%v", got, err)
		}
		_ = opened.Session.AbortJob(context.Background(), errors.New("cleanup"))
	})

	t.Run("closed discovery root", func(t *testing.T) {
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		_ = root.Close()
		candidate := outputTestID[transfer.OutputSessionID](85)
		name := outputJournalPrefix + encodeOutputFilenameToken(candidate.Bytes()) + ".journal"
		_, _, _, err = inspectOutputJournal(root, name, FilesystemOutputIntent{}, outputRootBinding{})
		if !errors.Is(err, ErrOutputDiscoveryUnsafe) {
			t.Fatalf("closed-root inspection error=%v", err)
		}
	})
}

func TestFilesystemOutputAuthorityRootAndCleanupDefenses(t *testing.T) {
	t.Run("invalid roots", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(file, []byte("x"), filePerm); err != nil {
			t.Fatal(err)
		}
		config := outputTestConfig(file)
		if _, err := outputAuthorityFor(t, 91).OpenOrCreate(context.Background(), outputDiscoveryIntent(config)); err == nil {
			t.Fatal("regular file admitted as output root")
		}
		config.RootPath = filepath.Join(t.TempDir(), strings.Repeat("x", 5000))
		if _, err := outputAuthorityFor(t, 92).OpenOrCreate(context.Background(), outputDiscoveryIntent(config)); !errors.Is(err, ErrPathTooLong) {
			t.Fatalf("overlong discovery root error=%v", err)
		}
	})

	t.Run("authority abandonment releases session lock", func(t *testing.T) {
		config := outputTestConfig(t.TempDir())
		session, err := NewFilesystemOutputSession(config)
		if err != nil {
			t.Fatal(err)
		}
		abandonFilesystemOutputSession(session)
		if _, _, err := session.BeginFile(context.Background(), outputTestFile("closed.bin", outputTestDescriptor(t, config, 93, 94, 1, catalog.ModifiedTime{}))); !errors.Is(err, ErrOutputSessionClosed) {
			t.Fatalf("abandoned session error=%v", err)
		}
		reopened, err := NewFilesystemOutputSession(config)
		if err != nil {
			t.Fatalf("abandoned lock remained active: %v", err)
		}
		_ = reopened.AbortJob(context.Background(), errors.New("cleanup"))
		abandonFilesystemOutputSession(nil)
	})

	t.Run("intent lock serializes discovery", func(t *testing.T) {
		config := outputTestConfig(t.TempDir())
		root, err := os.OpenRoot(config.RootPath)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		name := outputIntentLockName(config.ShareInstance, config.ResumeIntent)
		lock, err := acquireOutputSessionLock(root, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := outputAuthorityFor(t, 95).OpenOrCreate(context.Background(), outputDiscoveryIntent(config)); !errors.Is(err, ErrOutputSessionActive) {
			t.Fatalf("concurrent discovery error=%v", err)
		}
		if err := lock.close(true); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("cancellation precedes journal mutation", func(t *testing.T) {
		rootPath := t.TempDir()
		config := outputTestConfig(rootPath)
		candidate := outputTestID[transfer.OutputSessionID](96)
		name := outputJournalPrefix + encodeOutputFilenameToken(candidate.Bytes()) + ".journal"
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte("corrupt"), filePerm); err != nil {
			t.Fatal(err)
		}
		_, root, binding, err := openOutputDiscoveryRoot(rootPath)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := discoverMatchingOutputSessions(canceled, root, outputDiscoveryIntent(config), binding); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled classification error=%v", err)
		}
		if _, err := root.Lstat(name); err != nil {
			t.Fatalf("canceled discovery mutated journal: %v", err)
		}
	})
}

func TestJournalRangesContainSparseVerifiedState(t *testing.T) {
	available, err := content.NewRangeSet([]content.Range{{Offset: 0, End: 10}, {Offset: 20, End: 30}})
	if err != nil {
		t.Fatal(err)
	}
	required, _ := content.NewRangeSet([]content.Range{{Offset: 2, End: 8}, {Offset: 22, End: 29}})
	if !journalRangesContain(available, required) {
		t.Fatal("verified sparse subranges were not contained")
	}
	for _, ranges := range [][]content.Range{
		{{Offset: 8, End: 22}},
		{{Offset: 31, End: 32}},
	} {
		required, _ := content.NewRangeSet(ranges)
		if journalRangesContain(available, required) {
			t.Fatalf("unverified sparse range admitted: %v", ranges)
		}
	}
}
