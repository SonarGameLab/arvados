/*
 * Copyright (C) The Arvados Authors. All rights reserved.
 *
 * SPDX-License-Identifier: AGPL-3.0 OR Apache-2.0
 *
 */

package org.arvados.client.logic.keep;

import com.fasterxml.jackson.databind.ObjectMapper;
import org.arvados.client.api.client.CollectionsApiClient;
import org.arvados.client.api.client.KeepWebApiClient;
import org.arvados.client.api.model.Collection;
import org.arvados.client.common.Characters;
import org.arvados.client.logic.collection.FileToken;
import org.arvados.client.logic.collection.ManifestDecoder;
import org.arvados.client.logic.collection.ManifestStream;
import org.arvados.client.test.utils.FileTestUtils;
import org.arvados.client.utils.FileMerge;
import org.apache.commons.io.FileUtils;
import org.junit.After;
import org.junit.Assert;
import org.junit.Before;
import org.junit.Test;
import org.junit.runner.RunWith;
import org.mockito.InjectMocks;
import org.mockito.Mock;
import org.mockito.junit.MockitoJUnitRunner;

import java.io.File;
import java.io.IOException;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.UUID;

import static org.arvados.client.test.utils.FileTestUtils.*;
import static org.assertj.core.api.Assertions.assertThat;
import static org.mockito.Mockito.when;

@RunWith(MockitoJUnitRunner.class)
public class FileDownloaderTest {

    static final ObjectMapper MAPPER = new ObjectMapper().findAndRegisterModules();
    private Collection collectionToDownload;
    private ManifestStream manifestStream;

    @Mock
    private CollectionsApiClient collectionsApiClient;
    @Mock
    private KeepClient keepClient;
    @Mock
    private KeepWebApiClient keepWebApiClient;
    @Mock
    private ManifestDecoder manifestDecoder;
    @InjectMocks
    private FileDownloader fileDownloader;

    @Before
    public void setUp() throws Exception {
        FileTestUtils.createDirectory(FILE_SPLIT_TEST_DIR);
        FileTestUtils.createDirectory(FILE_DOWNLOAD_TEST_DIR);

        collectionToDownload = prepareCollection();
        manifestStream = prepareManifestStream();
    }

    @Test
    public void downloadingAllFilesFromCollectionWorksProperly() throws Exception {
        // given
        List<File> files = generatePredefinedFiles();
        byte[] dataChunk = prepareDataChunk(files);

        //having
        when(collectionsApiClient.get(collectionToDownload.getUuid())).thenReturn(collectionToDownload);
        when(manifestDecoder.decode(collectionToDownload.getManifestText())).thenReturn(Arrays.asList(manifestStream));
        when(keepClient.getDataChunk(manifestStream.getKeepLocators().get(0))).thenReturn(dataChunk);

        //when
        List<File> downloadedFiles = fileDownloader.downloadFilesFromCollection(collectionToDownload.getUuid(), FILE_DOWNLOAD_TEST_DIR);

        //then
        Assert.assertEquals(3, downloadedFiles.size()); // 3 files downloaded

        File collectionDir = new File(FILE_DOWNLOAD_TEST_DIR + Characters.SLASH + collectionToDownload.getUuid());
        Assert.assertTrue(collectionDir.exists()); // collection directory created

        // 3 files correctly saved
        assertThat(downloadedFiles).allMatch(File::exists);

        for(int i = 0; i < downloadedFiles.size(); i ++) {
            File downloaded = new File(collectionDir + Characters.SLASH + files.get(i).getName());
            Assert.assertArrayEquals(FileUtils.readFileToByteArray(downloaded), FileUtils.readFileToByteArray(files.get(i)));
        }
    }

    @Test
    public void downloadingSingleFileFromKeepWebWorksCorrectly() throws Exception{
        //given
        File file = generatePredefinedFiles().get(0);

        //having
        when(collectionsApiClient.get(collectionToDownload.getUuid())).thenReturn(collectionToDownload);
        when(manifestDecoder.decode(collectionToDownload.getManifestText())).thenReturn(Arrays.asList(manifestStream));
        when(keepWebApiClient.download(collectionToDownload.getUuid(), file.getName())).thenReturn(FileUtils.readFileToByteArray(file));

        //when
        File downloadedFile = fileDownloader.downloadSingleFileUsingKeepWeb(file.getName(), collectionToDownload.getUuid(), FILE_DOWNLOAD_TEST_DIR);

        //then
        Assert.assertTrue(downloadedFile.exists());
        Assert.assertEquals(file.getName(), downloadedFile.getName());
        Assert.assertArrayEquals(FileUtils.readFileToByteArray(downloadedFile), FileUtils.readFileToByteArray(file));
    }

    @After
    public void tearDown() throws Exception {
        FileTestUtils.cleanDirectory(FILE_SPLIT_TEST_DIR);
        FileTestUtils.cleanDirectory(FILE_DOWNLOAD_TEST_DIR);
    }

    private Collection prepareCollection() throws IOException {
        // collection that will be returned by mocked collectionsApiClient
        String filePath = "src/test/resources/org/arvados/client/api/client/collections-download-file.json";
        File jsonFile = new File(filePath);
        return MAPPER.readValue(jsonFile, Collection.class);
    }

    private ManifestStream prepareManifestStream() throws Exception {
        // manifestStream that will be returned by mocked manifestDecoder
        List<FileToken> fileTokens = new ArrayList<>();
        fileTokens.add(new FileToken("0:1024:test-file1"));
        fileTokens.add(new FileToken("1024:20480:test-file2"));
        fileTokens.add(new FileToken("21504:1048576:test-file\\0403"));

        KeepLocator keepLocator = new KeepLocator("163679d58edaadc28db769011728a72c+1070080+A3acf8c1fe582c265d2077702e4a7d74fcc03aba8@5aa4fdeb");
        return new ManifestStream(".", Arrays.asList(keepLocator), fileTokens);
    }

    private byte[] prepareDataChunk(List<File> files) throws IOException {
        File combinedFile = new File(FILE_SPLIT_TEST_DIR + Characters.SLASH + UUID.randomUUID());
        FileMerge.merge(files, combinedFile);
        return FileUtils.readFileToByteArray(combinedFile);
    }
}
