import { writeFile, mkdir } from 'node:fs/promises';
import { join } from 'node:path';

export function identity({ channel = 'development', publisher, version, architecture = 'x64' } = {}) {
  if (!['development', 'release'].includes(channel)) throw new Error('Unknown installation channel');
  if (!['x64', 'arm64'].includes(architecture)) throw new Error('Unsupported Explorer architecture');
  if (channel === 'release' && (!publisher || !version)) {
    throw new Error('Release packaging requires an explicit signing publisher and package version');
  }
  if (!version) {
    const now = new Date();
    const days = Math.floor(now.valueOf() / 86_400_000) - Math.floor(Date.UTC(2026, 0, 1) / 86_400_000);
    version = `0.1.${days}.${Math.floor((now.valueOf() % 86_400_000) / 2000)}`;
  }
  if (!/^\d+\.\d+\.\d+\.\d+$/u.test(version) || version.split('.').some(n => Number(n) > 65535)) {
    throw new Error('Package version must have four components in 0..65535');
  }
  return {
    channel, version, architecture,
    publisher: publisher ?? 'CN=WindShare Development',
    name: channel === 'release' ? 'WindShare.Desktop' : 'WindShare.Desktop.Development',
    applicationId: 'WindShare',
    clsid: channel === 'release' ? 'C4C39339-1ADE-4A87-AE3F-20DB91EAC9A1' : 'E09C97A5-7670-4317-877F-D14DBF33C9AB',
  };
}

const xml = value => value.replaceAll('&', '&amp;').replaceAll('"', '&quot;').replaceAll('<', '&lt;');

export async function writeIdentity(directory, configuration) {
  const id = Object.fromEntries(Object.entries(configuration).map(([key, value]) => [key, xml(value)]));
  await mkdir(directory, { recursive: true });
  await writeFile(join(directory, 'AppxManifest.xml'), `<?xml version="1.0" encoding="utf-8"?>
<Package xmlns="http://schemas.microsoft.com/appx/manifest/foundation/windows10"
 xmlns:uap="http://schemas.microsoft.com/appx/manifest/uap/windows10"
 xmlns:uap10="http://schemas.microsoft.com/appx/manifest/uap/windows10/10"
 xmlns:rescap="http://schemas.microsoft.com/appx/manifest/foundation/windows10/restrictedcapabilities"
 xmlns:com="http://schemas.microsoft.com/appx/manifest/com/windows10"
 xmlns:desktop4="http://schemas.microsoft.com/appx/manifest/desktop/windows10/4"
 xmlns:desktop5="http://schemas.microsoft.com/appx/manifest/desktop/windows10/5"
 IgnorableNamespaces="uap uap10 rescap com desktop4 desktop5">
 <Identity Name="${id.name}" Publisher="${id.publisher}" Version="${id.version}" ProcessorArchitecture="${id.architecture}" />
 <Properties>
  <DisplayName>WindShare</DisplayName><PublisherDisplayName>WindShare</PublisherDisplayName>
  <Logo>Assets\\logo-50.png</Logo><uap10:AllowExternalContent>true</uap10:AllowExternalContent>
 </Properties>
 <Resources><Resource Language="en-us" /><Resource Language="zh-cn" /></Resources>
 <Dependencies><TargetDeviceFamily Name="Windows.Desktop" MinVersion="10.0.22000.0" MaxVersionTested="10.0.26100.0" /></Dependencies>
 <Applications>
  <Application Id="${id.applicationId}" Executable="windshare.exe" uap10:TrustLevel="mediumIL" uap10:RuntimeBehavior="win32App">
   <uap:VisualElements DisplayName="WindShare" Description="WindShare" BackgroundColor="transparent"
    Square150x150Logo="Assets\\logo-150.png" Square44x44Logo="Assets\\logo-44.png" />
   <Extensions>
    <com:Extension Category="windows.comServer"><com:ComServer><com:SurrogateServer DisplayName="WindShare">
     <com:Class Id="${id.clsid}" Path="windshare_shell.dll" ThreadingModel="STA" />
    </com:SurrogateServer></com:ComServer></com:Extension>
    <desktop4:Extension Category="windows.fileExplorerContextMenus"><desktop4:FileExplorerContextMenus>
     <desktop5:ItemType Type="*"><desktop5:Verb Id="ShareWithWindShare" Clsid="${id.clsid}" /></desktop5:ItemType>
     <desktop5:ItemType Type="Directory"><desktop5:Verb Id="ShareWithWindShare" Clsid="${id.clsid}" /></desktop5:ItemType>
    </desktop4:FileExplorerContextMenus></desktop4:Extension>
    <uap:Extension Category="windows.shareTarget"><uap:ShareTarget>
     <uap:SupportedFileTypes><uap:SupportsAnyFileType /></uap:SupportedFileTypes>
     <uap:DataFormat>StorageItems</uap:DataFormat>
    </uap:ShareTarget></uap:Extension>
   </Extensions>
  </Application>
 </Applications>
 <Capabilities><rescap:Capability Name="runFullTrust" /><rescap:Capability Name="unvirtualizedResources" /></Capabilities>
</Package>
`);
  await writeFile(join(directory, 'windshare.manifest'), `<?xml version="1.0" encoding="utf-8"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
 <assemblyIdentity version="${id.version}" name="WindShare.Desktop" type="win32" />
 <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3"><security><requestedPrivileges>
  <requestedExecutionLevel level="asInvoker" uiAccess="false" />
 </requestedPrivileges></security></trustInfo>
 <application xmlns="urn:schemas-microsoft-com:asm.v3"><windowsSettings>
  <dpiAwareness xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">PerMonitorV2</dpiAwareness>
  <longPathAware xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">true</longPathAware>
 </windowsSettings></application>
 <msix xmlns="urn:schemas-microsoft-com:msix.v1" publisher="${id.publisher}" packageName="${id.name}" applicationId="${id.applicationId}" />
</assembly>
`);
  await writeFile(join(directory, 'identity.json'), JSON.stringify(configuration, null, 2) + '\n');
}
